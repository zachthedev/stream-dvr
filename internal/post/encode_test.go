package post

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// probeStartTimeout is how long two probe subprocesses get to appear.
//
// It bounds a wait for a liveness property rather than a speed one, so it
// only has to exceed how long the operating system takes to start two
// processes under load. See awaitProbes.
const probeStartTimeout = 30 * time.Second

// ffmpegEncoderList reproduces the shape of "ffmpeg -encoders" output: a
// header block, then one line per encoder with capability flags first.
const ffmpegEncoderList = `Encoders:
 V..... = Video
 ------
 V....D av1_nvenc            NVIDIA NVENC av1 encoder
 V....D libsvtav1            SVT-AV1 encoder
 V....D hevc_nvenc           NVIDIA NVENC hevc encoder
 V....D libx265              libx265 H.265 / HEVC
 A....D aac                  AAC (Advanced Audio Coding)
`

// softwareOnlyList has no hardware encoders.
const softwareOnlyList = `Encoders:
 V....D libsvtav1            SVT-AV1 encoder
 V....D libx265              libx265 H.265 / HEVC
`

// encoderNames extracts names for comparison.
func encoderNames(encoders []Encoder) []string {
	names := make([]string, 0, len(encoders))
	for _, encoder := range encoders {
		names = append(names, encoder.Name)
	}
	return names
}

// ///////////////////////////////////////////////
// Encoder catalogue
// ///////////////////////////////////////////////

func TestKnownEncoders_CoversEveryPlatformsHardware(t *testing.T) {
	// An absent entry is not a missing optimisation. Every hardware probe
	// fails, SelectEncoder falls to software, and a four-hour capture takes
	// six to sixteen hours on the CPU instead of under one.
	//
	// The catalogue is a list of candidates rather than a claim: Encoders
	// verifies each by actually encoding a frame, so an entry a machine
	// cannot run is dropped rather than believed.
	tests := []struct {
		name    string
		encoder string
		reason  string
	}{
		{
			name:    "NVIDIA",
			encoder: "hevc_nvenc",
			reason:  "Windows and Linux with a proprietary driver",
		},
		{
			name:    "Intel Quick Sync",
			encoder: "hevc_qsv",
			reason:  "Windows with an Intel iGPU",
		},
		{
			name:    "AMD",
			encoder: "hevc_amf",
			reason:  "Windows and Linux with an AMD GPU",
		},
		{
			// nvenc needs a driver Apple dropped, amf is Windows and Linux,
			// and qsv needs a runtime that does not build on macOS.
			name:    "Apple",
			encoder: "hevc_videotoolbox",
			reason:  "the only hardware encoder macOS has",
		},
		{
			// The encoder that works on a stock install, where nvenc needs
			// a proprietary driver and amf needs a separate runtime.
			name:    "VAAPI",
			encoder: "hevc_vaapi",
			reason:  "Intel and AMD on Linux",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, encoder := range knownEncoders {
				if encoder.Name != tt.encoder {
					continue
				}
				if !encoder.Hardware {
					t.Errorf("%s is not marked as hardware, so PreferHardware skips it", tt.encoder)
				}
				if encoder.qualityFlag == "" {
					// A missing flag is ignored silently and produces a
					// default-bitrate encode, which is not what was asked
					// for and never says so.
					t.Errorf("%s declares no quality flag", tt.encoder)
				}
				return
			}
			t.Errorf("knownEncoders has no %s, which is %s", tt.encoder, tt.reason)
		})
	}
}

func TestKnownEncoders_OffersNoAV1OnApple(t *testing.T) {
	// No Apple silicon has an AV1 encoder. M3 and M4 added AV1 decode only,
	// and ffmpeg has no av1_videotoolbox at all, so an entry for one would
	// be selected, probed, and dropped on every Mac forever.
	for _, encoder := range knownEncoders {
		if encoder.Name == "av1_videotoolbox" {
			t.Error("knownEncoders offers av1_videotoolbox, which ffmpeg does not have")
		}
	}
}

// ///////////////////////////////////////////////
// Encoders
// ///////////////////////////////////////////////

func TestEncoders_ReadsWhatFFmpegOffers(t *testing.T) {
	// Availability is read from ffmpeg rather than assumed, because a
	// build can omit encoders the hardware supports and a machine can lack
	// the GPU its build was compiled for.
	fakeExec(t, "encoders", "FAKE_ENCODERS="+ffmpegEncoderList)

	got, err := fakePipeline().Encoders(context.Background())
	if err != nil {
		t.Fatalf("Encoders() err = %v, want nil", err)
	}

	want := []string{"av1_nvenc", "libsvtav1", "hevc_nvenc", "libx265"}
	names := encoderNames(got)
	if len(names) != len(want) {
		t.Fatalf("Encoders() = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Errorf("Encoders()[%d] = %q, want %q (catalogue order, hardware first)", i, names[i], name)
		}
	}
}

func TestEncoders_IgnoresUnknownEntries(t *testing.T) {
	fakeExec(t, "encoders", "FAKE_ENCODERS="+softwareOnlyList)

	got, err := fakePipeline().Encoders(context.Background())
	if err != nil {
		t.Fatalf("Encoders() err = %v, want nil", err)
	}
	for _, encoder := range got {
		if encoder.Hardware {
			t.Errorf("Encoders() returned hardware encoder %q from a software-only build", encoder.Name)
		}
	}
}

func TestEncoders_DropsAnEncoderThatCannotRun(t *testing.T) {
	// "ffmpeg -encoders" lists what was compiled in, not what the hardware
	// can run. On an RTX 3080 av1_nvenc is listed and fails with "No
	// capable devices found", because Ampere has no AV1 encoder. Selecting
	// from the list alone picks an encoder that dies at the first
	// transcode, hours into a broadcast.
	fakeExec(t, "encoders_selective_probe",
		"FAKE_ENCODERS="+ffmpegEncoderList,
		"FAKE_BROKEN_ENCODER=av1_nvenc")

	got, err := fakePipeline().Encoders(context.Background())
	if err != nil {
		t.Fatalf("Encoders() err = %v, want nil", err)
	}

	names := encoderNames(got)
	for _, name := range names {
		if name == "av1_nvenc" {
			t.Errorf("Encoders() = %v, want the unusable encoder dropped", names)
		}
	}
	// The rest of the build must survive the one failure.
	if !slices.Contains(names, "hevc_nvenc") {
		t.Errorf("Encoders() = %v, want the working hardware encoder kept", names)
	}
	if !slices.Contains(names, "libsvtav1") {
		t.Errorf("Encoders() = %v, want the software encoders kept", names)
	}
}

func TestEncoders_SelectionSkipsTheUnusableEncoder(t *testing.T) {
	// The end-to-end consequence: with av1_nvenc unusable, asking for AV1
	// with hardware preferred has to fall to software rather than
	// returning a broken encoder.
	fakeExec(t, "encoders_selective_probe",
		"FAKE_ENCODERS="+ffmpegEncoderList,
		"FAKE_BROKEN_ENCODER=av1_nvenc")

	available, err := fakePipeline().Encoders(context.Background())
	if err != nil {
		t.Fatalf("Encoders() err = %v, want nil", err)
	}

	got, softwareFallback, err := SelectEncoder(available, TranscodeOptions{
		Codec: config.CodecAV1, PreferHardware: true,
	})
	if err != nil {
		t.Fatalf("SelectEncoder() err = %v, want nil", err)
	}
	if got.Name != "libsvtav1" {
		t.Errorf("SelectEncoder() = %q, want it to fall back to software", got.Name)
	}
	// Silence here costs hours per broadcast with nothing said about why.
	if !softwareFallback {
		t.Error("SelectEncoder() reported no fallback, want hardware-preferred software said out loud")
	}
}

func TestEncoders_CachesTheProbe(t *testing.T) {
	// Probing costs a subprocess per candidate, and capability only
	// changes with a driver or ffmpeg upgrade, so the result is resolved
	// once per process rather than per transcode.
	fakeExec(t, "encoders", "FAKE_ENCODERS="+ffmpegEncoderList)
	pipeline := fakePipeline()

	first, err := pipeline.Encoders(context.Background())
	if err != nil {
		t.Fatalf("Encoders() err = %v, want nil", err)
	}

	// A later failure must not be observed, because nothing re-probes.
	fakeExec(t, "fails")
	second, err := pipeline.Encoders(context.Background())
	if err != nil {
		t.Fatalf("second Encoders() err = %v, want the cached result", err)
	}
	if len(second) != len(first) {
		t.Errorf("second Encoders() returned %d, want the cached %d", len(second), len(first))
	}
}

func TestEncoders_ProbeFailure(t *testing.T) {
	fakeExec(t, "fails")

	if _, err := fakePipeline().Encoders(context.Background()); err == nil {
		t.Error("Encoders() err = nil, want an error")
	}
}

func TestEncoders_CachesAnAnswerOnlyWhenItFoundSomething(t *testing.T) {
	// Caching an empty answer commits every remaining transcode in the
	// process to software, at hours per broadcast, because one GPU was busy
	// for one moment. Nothing short of a restart undoes it.
	tests := []struct {
		name         string
		wantEncoders bool
		wantReprobe  bool
	}{
		{
			name:        "nothing usable is re-probed",
			wantReprobe: true,
		},
		{
			name:         "a usable set is kept",
			wantEncoders: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			if tt.wantEncoders {
				fakeExec(t, "encoders", "FAKE_ENCODERS="+ffmpegEncoderList)
			} else {
				// Every capability probe fails, which is what a driver
				// mid-upgrade or a GPU already busy looks like.
				fakeExec(t, "encoders_selective_probe",
					"FAKE_ENCODERS="+ffmpegEncoderList,
					"FAKE_BROKEN_ENCODER=-c:v")
			}
			countCalls(t, &calls)

			pipeline := fakePipeline()
			first, err := pipeline.Encoders(context.Background())
			if err != nil {
				t.Fatalf("Encoders() err = %v, want nil", err)
			}
			if got := len(first) > 0; got != tt.wantEncoders {
				t.Fatalf("Encoders() = %v, want any = %t", encoderNames(first), tt.wantEncoders)
			}

			after := calls.Load()
			if _, err := pipeline.Encoders(context.Background()); err != nil {
				t.Fatalf("second Encoders() err = %v, want nil", err)
			}

			reprobed := calls.Load() > after
			if reprobed != tt.wantReprobe {
				t.Errorf("second Encoders() re-probed = %t, want %t", reprobed, tt.wantReprobe)
			}
		})
	}
}

// countCalls counts every subprocess the pipeline starts from here on.
func countCalls(t *testing.T, calls *atomic.Int64) {
	t.Helper()

	fake := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls.Add(1)
		return fake(ctx, name, args...)
	}
}

// ///////////////////////////////////////////////
// SelectEncoder
// ///////////////////////////////////////////////

func TestSelectEncoder(t *testing.T) {
	all := []Encoder{
		{Name: "av1_nvenc", Codec: config.CodecAV1, Hardware: true},
		{Name: "libsvtav1", Codec: config.CodecAV1, Hardware: false},
		{Name: "hevc_nvenc", Codec: config.CodecHEVC, Hardware: true},
		{Name: "libx265", Codec: config.CodecHEVC, Hardware: false},
	}
	softwareOnly := []Encoder{
		{Name: "libsvtav1", Codec: config.CodecAV1, Hardware: false},
	}
	hardwareOnly := []Encoder{
		{Name: "av1_nvenc", Codec: config.CodecAV1, Hardware: true},
	}

	tests := []struct {
		name                 string
		available            []Encoder
		opts                 TranscodeOptions
		want                 string
		wantSoftwareFallback bool
	}{
		{
			name:      "hardware preferred and present",
			available: all,
			opts:      TranscodeOptions{Codec: config.CodecAV1, PreferHardware: true},
			want:      "av1_nvenc",
		},
		{
			// Software was what was asked for, so there is nothing to
			// report.
			name:      "hardware declined falls to software",
			available: all,
			opts:      TranscodeOptions{Codec: config.CodecAV1, PreferHardware: false},
			want:      "libsvtav1",
		},
		{
			// Software encoding a multi-hour broadcast runs for hours, but
			// running for hours beats not transcoding at all. It is still
			// an order of magnitude slower than what was asked for.
			name:                 "hardware preferred but absent",
			available:            softwareOnly,
			opts:                 TranscodeOptions{Codec: config.CodecAV1, PreferHardware: true},
			want:                 "libsvtav1",
			wantSoftwareFallback: true,
		},
		{
			name:      "software preferred but absent",
			available: hardwareOnly,
			opts:      TranscodeOptions{Codec: config.CodecAV1, PreferHardware: false},
			want:      "av1_nvenc",
		},
		{
			name:      "codec selects the family",
			available: all,
			opts:      TranscodeOptions{Codec: config.CodecHEVC, PreferHardware: true},
			want:      "hevc_nvenc",
		},
		{
			// No Apple silicon has an AV1 encoder and ffmpeg has no
			// av1_videotoolbox, so a Mac configured for AV1 always lands
			// here however much hardware it has.
			name: "a Mac asked for AV1 gets software and hears about it",
			available: []Encoder{
				{Name: "hevc_videotoolbox", Codec: config.CodecHEVC, Hardware: true},
				{Name: "libsvtav1", Codec: config.CodecAV1, Hardware: false},
			},
			opts:                 TranscodeOptions{Codec: config.CodecAV1, PreferHardware: true},
			want:                 "libsvtav1",
			wantSoftwareFallback: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, softwareFallback, err := SelectEncoder(tt.available, tt.opts)
			if err != nil {
				t.Fatalf("SelectEncoder() err = %v, want nil", err)
			}
			if got.Name != tt.want {
				t.Errorf("SelectEncoder() = %q, want %q", got.Name, tt.want)
			}
			if softwareFallback != tt.wantSoftwareFallback {
				t.Errorf("SelectEncoder() software fallback = %t, want %t",
					softwareFallback, tt.wantSoftwareFallback)
			}
		})
	}
}

func TestSelectEncoder_NoneForCodec(t *testing.T) {
	available := []Encoder{{Name: "libx265", Codec: config.CodecHEVC}}

	if _, _, err := SelectEncoder(available, TranscodeOptions{Codec: config.CodecAV1}); err == nil {
		t.Error("SelectEncoder() err = nil, want an error when the codec has no encoder")
	}
}

func TestSelectEncoder_EmptySet(t *testing.T) {
	if _, _, err := SelectEncoder(nil, TranscodeOptions{Codec: config.CodecAV1}); err == nil {
		t.Error("SelectEncoder() err = nil, want an error")
	}
}

// ///////////////////////////////////////////////
// Transcode
// ///////////////////////////////////////////////

func TestTranscode(t *testing.T) {
	fakeExec(t, "write_output")
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "source.mkv"))
	output := filepath.Join(dir, "smaller.mkv")

	encoder := Encoder{Name: "av1_nvenc", Codec: config.CodecAV1, Hardware: true, qualityFlag: "-cq"}
	if err := fakePipeline().Transcode(context.Background(), source, output, encoder, 30); err != nil {
		t.Fatalf("Transcode() err = %v, want nil", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Errorf("transcode output missing: %v", err)
	}
}

func TestTranscode_RejectsQualityOutsideRange(t *testing.T) {
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "source.mkv"))
	encoder := Encoder{Name: "libsvtav1", Codec: config.CodecAV1, qualityFlag: "-crf"}

	for _, quality := range []int{config.MinQuality - 1, config.MaxQuality + 1} {
		if err := fakePipeline().Transcode(context.Background(), source,
			filepath.Join(dir, "out.mkv"), encoder, quality); err == nil {
			t.Errorf("Transcode() with quality %d err = nil, want a rejection", quality)
		}
	}
}

func TestTranscode_RefusesToOverwrite(t *testing.T) {
	fakeExec(t, "write_output")
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "source.mkv"))
	output := touch(t, filepath.Join(dir, "smaller.mkv"))

	encoder := Encoder{Name: "libsvtav1", Codec: config.CodecAV1, qualityFlag: "-crf"}
	err := fakePipeline().Transcode(context.Background(), source, output, encoder, 30)
	if err == nil {
		t.Fatal("Transcode() err = nil, want a refusal to overwrite")
	}
	if !strings.Contains(err.Error(), output) {
		t.Errorf("Transcode() err = %q, want it to name the occupied path", err)
	}
}

func TestTranscode_RemovesAPartialOutputOnFailure(t *testing.T) {
	fakeExec(t, "write_output_then_fail")
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "source.mkv"))
	output := filepath.Join(dir, "smaller.mkv")

	encoder := Encoder{Name: "libsvtav1", Codec: config.CodecAV1, qualityFlag: "-crf"}
	if err := fakePipeline().Transcode(context.Background(), source, output, encoder, 30); err == nil {
		t.Fatal("Transcode() err = nil, want the failure surfaced")
	}
	if _, err := os.Stat(output); err == nil {
		t.Error("partial transcode output survived, want it removed")
	}
}

func TestTranscode_Arguments(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fakeExec(t, "record_args", "FAKE_ARGS_FILE="+argsFile)

	source := touch(t, filepath.Join(dir, "source.mkv"))
	output := filepath.Join(dir, "smaller.mkv")
	encoder := Encoder{
		Name: "av1_nvenc", Codec: config.CodecAV1, Hardware: true,
		qualityFlag: "-cq", extraArgs: []string{"-preset", "p5"},
	}

	if err := fakePipeline().Transcode(context.Background(), source, output, encoder, 30); err != nil {
		// The helper records arguments in place of writing output, so the
		// missing file is expected. Only the recorded arguments matter.
		t.Logf("Transcode() err = %v (output intentionally not written)", err)
	}

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded arguments: %v", err)
	}
	joined := strings.Join(strings.Split(string(recorded), "\n"), " ")

	// The quality flag differs per encoder family, and passing the wrong
	// one is silently ignored, yielding a default-bitrate encode that
	// saves nothing.
	for _, want := range []string{"-c:v av1_nvenc", "-cq 30", "-preset p5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("transcode arguments = %q, want them to contain %q", joined, want)
		}
	}

	// Audio is already a small share of the bitrate, so re-encoding it
	// adds a lossy generation for a saving that does not register.
	if !strings.Contains(joined, "-c:a copy") {
		t.Errorf("transcode arguments = %q, want audio copied rather than re-encoded", joined)
	}
}

func TestTranscode_SelectsTheSameStreamsAsRemux(t *testing.T) {
	// Both are passed through the same verification, which demands every
	// kind the source carried. A step that selects fewer streams than that
	// gate accepts can never clear it, and the encode that produced the
	// output has already burned hours by then.
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "source.mkv"))

	remuxArgs := filepath.Join(dir, "remux.txt")
	fakeExec(t, "record_args", "FAKE_ARGS_FILE="+remuxArgs)
	if err := fakePipeline().Remux(context.Background(), source, filepath.Join(dir, "remuxed.mkv")); err != nil {
		t.Logf("Remux() err = %v (the helper records arguments instead of writing output)", err)
	}

	transcodeArgs := filepath.Join(dir, "transcode.txt")
	fakeExec(t, "record_args", "FAKE_ARGS_FILE="+transcodeArgs)
	encoder := Encoder{Name: "av1_nvenc", Codec: config.CodecAV1, qualityFlag: "-cq"}
	if err := fakePipeline().Transcode(context.Background(), source,
		filepath.Join(dir, "smaller.mkv"), encoder, 30); err != nil {
		t.Logf("Transcode() err = %v (the helper records arguments instead of writing output)", err)
	}

	remuxed, transcoded := selectedStreams(t, remuxArgs), selectedStreams(t, transcodeArgs)
	if !slices.Equal(remuxed, transcoded) {
		t.Errorf("remux selects %v and transcode selects %v, want one statement of what must survive",
			remuxed, transcoded)
	}
	if len(remuxed) == 0 {
		t.Fatal("neither step selected any stream, so the comparison proved nothing")
	}
}

// selectedStreams returns the stream specifiers a recorded command mapped.
func selectedStreams(t *testing.T, argsFile string) []string {
	t.Helper()

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded arguments: %v", err)
	}

	args := strings.Split(string(recorded), "\n")
	var selected []string
	for i, arg := range args {
		if arg == "-map" && i+1 < len(args) {
			selected = append(selected, args[i+1])
		}
	}
	slices.Sort(selected)
	return selected
}

func TestEncoders_ProbesWithoutHoldingTheCache(t *testing.T) {
	// Probing runs a subprocess per candidate with no bound on how long a
	// driver takes to answer. Holding the cache across that leaves every
	// caller in the process waiting on one hung encoder.
	probes := t.TempDir()
	barrier := filepath.Join(t.TempDir(), "release")
	fakeExec(t, "encoders_concurrent",
		"FAKE_ENCODERS= V....D av1_nvenc  NVIDIA AV1\n",
		"FAKE_PROBE_DIR="+probes,
		"FAKE_BARRIER="+barrier)

	pipeline := fakePipeline()
	var callers sync.WaitGroup
	for range 2 {
		callers.Go(func() {
			if _, err := pipeline.Encoders(context.Background()); err != nil {
				t.Errorf("Encoders() err = %v, want nil", err)
			}
		})
	}

	started := awaitProbes(t, probes, 2)

	if err := os.WriteFile(barrier, []byte("go"), 0o644); err != nil {
		t.Fatalf("releasing the probes: %v", err)
	}
	callers.Wait()

	if started < 2 {
		t.Errorf("%d probe ran at once, want both callers probing rather than one waiting on the other", started)
	}
}

// awaitProbes waits for want probes to announce themselves and returns how
// many did.
//
// The bound is generous on purpose. What is under test is that the second
// probe starts at all, not that it starts quickly. A cache lock held across
// the subprocess blocks it until the first one finishes, and the first is
// waiting on a barrier this test has not released, so a broken
// implementation never reaches two however long anyone waits. A tight bound
// measures how loaded the machine is. One failed this test under the full
// parallel suite.
func awaitProbes(t *testing.T, dir string, want int) int {
	t.Helper()

	deadline := time.Now().Add(probeStartTimeout)
	for {
		started := running(t, dir)
		if started >= want || time.Now().After(deadline) {
			return started
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// running counts the probes that have announced themselves.
func running(t *testing.T, dir string) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return len(entries)
}

func TestTranscode_QualityFlagMatchesTheEncoder(t *testing.T) {
	tests := []struct {
		name    string
		encoder Encoder
		want    string
	}{
		{
			name:    "nvenc takes cq",
			encoder: Encoder{Name: "av1_nvenc", Codec: config.CodecAV1, qualityFlag: "-cq"},
			want:    "-cq 30",
		},
		{
			name:    "svt-av1 takes crf",
			encoder: Encoder{Name: "libsvtav1", Codec: config.CodecAV1, qualityFlag: "-crf"},
			want:    "-crf 30",
		},
		{
			name:    "qsv takes global_quality",
			encoder: Encoder{Name: "av1_qsv", Codec: config.CodecAV1, qualityFlag: "-global_quality"},
			want:    "-global_quality 30",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args.txt")
			fakeExec(t, "record_args", "FAKE_ARGS_FILE="+argsFile)

			source := touch(t, filepath.Join(dir, "source.mkv"))
			_ = fakePipeline().Transcode(context.Background(), source,
				filepath.Join(dir, "out.mkv"), tt.encoder, 30)

			recorded, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("reading recorded arguments: %v", err)
			}
			joined := strings.Join(strings.Split(string(recorded), "\n"), " ")
			if !strings.Contains(joined, tt.want) {
				t.Errorf("transcode arguments = %q, want %q", joined, tt.want)
			}
		})
	}
}

func TestKnownEncoders_CoverEveryConfiguredCodec(t *testing.T) {
	// A codec accepted by config with no encoder behind it would validate
	// fine and then fail at the first transcode.
	for _, codec := range config.RecompressCodecs {
		t.Run(codec, func(t *testing.T) {
			var hardware, software bool
			for _, encoder := range knownEncoders {
				if encoder.Codec != codec {
					continue
				}
				if encoder.Hardware {
					hardware = true
				} else {
					software = true
				}
			}
			if !hardware {
				t.Errorf("codec %q has no hardware encoder", codec)
			}
			if !software {
				t.Errorf("codec %q has no software fallback", codec)
			}
		})
	}
}
