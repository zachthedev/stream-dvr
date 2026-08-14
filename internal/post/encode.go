package post

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Encoder describes one way ffmpeg can produce a codec.
type Encoder struct {
	// Name is ffmpeg's encoder name, such as "av1_nvenc".
	Name string
	// Codec is the config codec this encoder satisfies.
	Codec string
	// Hardware reports whether it runs on a GPU. Hardware encoding is far
	// faster and somewhat larger at the same perceived quality.
	Hardware bool
	// qualityFlag is the argument this encoder takes its constant-quality
	// level through. The name differs per encoder family, and passing the
	// wrong one is silently ignored, producing a default-bitrate encode.
	qualityFlag string
	// extraArgs tune speed against size for this encoder.
	extraArgs []string
}

// TranscodeOptions selects how a recording is re-encoded.
type TranscodeOptions struct {
	// Codec is the target, one of config.RecompressCodecs.
	Codec string
	// Quality is the constant-quality level. Lower is better and larger.
	Quality int
	// PreferHardware picks a GPU encoder when one is available.
	PreferHardware bool
}

// ///////////////////////////////////////////////
// Detection
// ///////////////////////////////////////////////

// probeSize is the frame the capability check encodes. Hardware encoders
// reject very small frames, so this is comfortably above their minimums
// while still costing a fraction of a second.
const probeSize = "320x240"

// encoderProbeTimeout bounds one capability probe. A driver in a bad state
// can leave an encoder open forever, and detection runs on the way to a
// transcode rather than in a place anyone is watching.
const encoderProbeTimeout = 30 * time.Second

// ///////////////////////////////////////////////
// Encoder catalogue
// ///////////////////////////////////////////////

// knownEncoders lists every encoder the pipeline can drive, best first
// within each codec. Hardware entries lead because a software AV1 encode of
// a multi-hour broadcast runs for hours.
var knownEncoders = []Encoder{
	{
		Name: "av1_nvenc", Codec: config.CodecAV1, Hardware: true,
		qualityFlag: "-cq", extraArgs: []string{"-preset", "p5"},
	},
	{
		Name: "av1_qsv", Codec: config.CodecAV1, Hardware: true,
		qualityFlag: "-global_quality", extraArgs: []string{"-preset", "medium"},
	},
	{
		Name: "av1_amf", Codec: config.CodecAV1, Hardware: true,
		qualityFlag: "-qp_p", extraArgs: []string{"-quality", "balanced"},
	},
	{
		// VAAPI is what works on stock Intel and AMD Linux, where nvenc
		// needs a proprietary driver and amf needs a separate runtime.
		Name: "av1_vaapi", Codec: config.CodecAV1, Hardware: true,
		qualityFlag: "-qp", extraArgs: []string{"-rc_mode", "CQP"},
	},
	{
		Name: "libsvtav1", Codec: config.CodecAV1, Hardware: false,
		qualityFlag: "-crf", extraArgs: []string{"-preset", "6"},
	},
	{
		Name: "hevc_nvenc", Codec: config.CodecHEVC, Hardware: true,
		qualityFlag: "-cq", extraArgs: []string{"-preset", "p5"},
	},
	{
		Name: "hevc_qsv", Codec: config.CodecHEVC, Hardware: true,
		qualityFlag: "-global_quality", extraArgs: []string{"-preset", "medium"},
	},
	{
		Name: "hevc_amf", Codec: config.CodecHEVC, Hardware: true,
		qualityFlag: "-qp_p", extraArgs: []string{"-quality", "balanced"},
	},
	{
		// The only hardware encoder macOS has. nvenc needs a driver Apple
		// dropped, amf is Windows and Linux, and qsv needs a runtime that
		// does not build there, so without this every hardware probe on a
		// Mac fails and every transcode runs on the CPU.
		Name: "hevc_videotoolbox", Codec: config.CodecHEVC, Hardware: true,
		qualityFlag: "-q:v", extraArgs: []string{"-tag:v", "hvc1"},
	},
	{
		Name: "hevc_vaapi", Codec: config.CodecHEVC, Hardware: true,
		qualityFlag: "-qp", extraArgs: []string{"-rc_mode", "CQP"},
	},
	{
		Name: "libx265", Codec: config.CodecHEVC, Hardware: false,
		qualityFlag: "-crf", extraArgs: []string{"-preset", "medium"},
	},
}

// Encoders returns the encoders this ffmpeg build can actually run, best
// first.
//
// Listing is not enough. An encoder appears in "ffmpeg -encoders" when it
// was compiled in, which says nothing about whether the hardware and driver
// on this machine can run it. On an RTX 3080 both av1_nvenc and hevc_nvenc
// are listed, yet av1_nvenc fails with "No capable devices found", because
// Ampere has no AV1 encoder. A driver whose nvenc API version disagrees
// with the ffmpeg build fails both the same way, which this machine did
// until a driver update. Selecting from the list alone would pick an
// encoder that fails at the first transcode, hours into a broadcast.
//
// So every candidate is verified by encoding a single frame. The result is
// cached for the process, because capability only changes with a driver or
// ffmpeg upgrade and neither happens without a restart in practice.
func (p *Pipeline) Encoders(ctx context.Context) ([]Encoder, error) {
	if cached := p.cachedEncoders(); cached != nil {
		return cached, nil
	}

	binary, err := p.ffmpeg()
	if err != nil {
		return nil, err
	}

	output, err := toolOutput(ctx, binary, "-hide_banner", "-encoders")
	if err != nil {
		return nil, fmt.Errorf("listing ffmpeg encoders: %w", err)
	}

	present := make(map[string]bool)
	for line := range strings.SplitSeq(output, "\n") {
		// Each entry reads " V....D name  description", so the name is the
		// second field once the capability flags are consumed.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		present[fields[1]] = true
	}

	// Listing and probing run outside the lock. Every reader of the cache
	// takes it, and one encoder that never returns would otherwise hold
	// every caller in the process behind it.
	var available []Encoder
	for _, encoder := range knownEncoders {
		if present[encoder.Name] && p.usable(ctx, binary, encoder.Name) {
			available = append(available, encoder)
		}
	}

	// An empty answer is not cached. A GPU busy with something else, a
	// driver mid-upgrade, or a cancelled sweep all produce one, and keeping
	// it would commit every remaining transcode in this process to software
	// at hours per broadcast. Re-probing costs one round of short
	// subprocesses the next time a transcode is due.
	if len(available) == 0 {
		return nil, nil
	}

	p.encoderMu.Lock()
	defer p.encoderMu.Unlock()
	// Two callers probing at once is worth the duplicated subprocesses.
	// Two different answers to the same question is not, so the first
	// result published is the one the process keeps.
	if p.encoders == nil {
		p.encoders = available
	}
	return p.encoders, nil
}

// cachedEncoders returns the verified encoder set, or nil until a call has
// finished probing.
func (p *Pipeline) cachedEncoders() []Encoder {
	p.encoderMu.Lock()
	defer p.encoderMu.Unlock()
	return p.encoders
}

// usable reports whether an encoder can encode a frame on this machine.
func (p *Pipeline) usable(ctx context.Context, binary, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, encoderProbeTimeout)
	defer cancel()

	cmd := command(ctx, binary,
		"-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size="+probeSize+":rate=1:duration=1",
		"-c:v", name,
		"-f", "null", "-")
	return procgroup.Run(cmd) == nil
}

// SelectEncoder picks the encoder to use for a codec.
//
// Software is chosen only when no hardware encoder exists or hardware is
// declined, because the difference is hours of encoding per broadcast.
//
// The second return reports that hardware was asked for and software is
// what there was. It is not an error, because a slow transcode beats none,
// but it has to be said out loud: a four-hour 1080p60 capture is 864,000
// frames, and libx265 at preset medium runs 15 to 40 frames a second, so
// the job takes between six and sixteen hours. The same encode on a
// hardware encoder finishes inside one. A Mac asked for AV1 always lands
// here, because no Apple silicon has an AV1 encoder and ffmpeg has no
// av1_videotoolbox to offer.
func SelectEncoder(available []Encoder, opts TranscodeOptions) (Encoder, bool, error) {
	var software Encoder
	for _, encoder := range available {
		if encoder.Codec != opts.Codec {
			continue
		}
		if encoder.Hardware {
			if opts.PreferHardware {
				return encoder, false, nil
			}
			continue
		}
		if software.Name == "" {
			software = encoder
		}
	}

	if software.Name != "" {
		return software, opts.PreferHardware, nil
	}

	// Falling back to hardware when software was preferred beats
	// refusing to transcode at all.
	for _, encoder := range available {
		if encoder.Codec == opts.Codec {
			return encoder, false, nil
		}
	}
	return Encoder{}, false, fmt.Errorf("no encoder available for %s", opts.Codec)
}

// ///////////////////////////////////////////////
// Transcode
// ///////////////////////////////////////////////

// Transcode re-encodes a recording's video to a denser codec.
//
// Only video is re-encoded. Stream audio is already a modest share of the
// bitrate, so re-encoding it would add a lossy generation for a saving that
// does not register, and everything else carries through untouched because
// verification requires every kind the source had.
//
// This does not delete the source. Pass it through ReplaceVerified so the
// original survives unless the output proves faithful.
func (p *Pipeline) Transcode(ctx context.Context, source, output string, encoder Encoder, quality int) error {
	binary, err := p.ffmpeg()
	if err != nil {
		return err
	}
	if err := paths.RequireAbsent("output", output); err != nil {
		return err
	}
	if quality < config.MinQuality || quality > config.MaxQuality {
		return fmt.Errorf("quality %d is outside %d to %d", quality, config.MinQuality, config.MaxQuality)
	}

	args := []string{
		"-hide_banner", "-v", "error",
		"-i", source,
	}
	args = append(args, mapArgs()...)
	args = append(args,
		"-c:v", encoder.Name,
		encoder.qualityFlag, strconv.Itoa(quality),
	)
	args = append(args, encoder.extraArgs...)
	args = append(args, "-c:a", "copy", "-c:s", "copy", output)

	cmd := command(ctx, binary, args...)
	stderr := procgroup.NewOutput(maxCapturedOutput)
	cmd.Stderr = stderr

	if err := procgroup.Run(cmd); err != nil {
		discard(ctx, output)
		return fmt.Errorf("transcoding %s with %s: %w: %s",
			source, encoder.Name, err, stderr.Excerpt(procgroup.MaxErrorText))
	}
	return nil
}
