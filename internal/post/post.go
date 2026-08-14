// Package post runs the steps that turn a finished capture into a library
// file: remuxing out of MPEG-TS, verifying the result, and re-encoding
// older recordings to reclaim space.
//
// Every step that replaces a file verifies the replacement before the
// source is eligible for deletion. Verification compares stream
// inventories, compares durations, and reads every packet of both files,
// so a source that cannot be read whole is never used as a yardstick. A
// step that cannot prove its output is faithful leaves the source in place
// and reports the failure. It never deletes on optimism.
package post

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Pipeline runs post-processing steps against media files.
type Pipeline struct {
	// ffmpeg and ffprobe resolve per call. A package upgrade renames the
	// versioned directory these live in, so a path cached at startup goes
	// stale without warning. Neither takes a context, because locating a
	// file starts nothing that could be cancelled.
	ffmpeg  func() (string, error)
	ffprobe func() (string, error)

	// encoders caches the verified encoder set. Capability only changes
	// with a driver or ffmpeg upgrade, and probing costs one subprocess
	// per candidate, so it is resolved once per process.
	encoderMu sync.Mutex
	encoders  []Encoder
}

// VerifyError reports output that could not be shown faithful to its
// source. The source is always still on disk when this is returned.
type VerifyError struct {
	// Source and Output are the compared files.
	Source string
	Output string
	// Reason says which check failed.
	Reason string
}

// streamKind pairs the name ffprobe reports for a kind of stream with the
// specifier ffmpeg's -map takes for it.
type streamKind struct {
	name      string
	specifier string
}

// streamInventory is the part of ffprobe's JSON this package reads.
type streamInventory struct {
	Streams []inventoryStream `json:"streams"`
}

// inventoryStream is one entry of an ffprobe stream listing.
type inventoryStream struct {
	CodecType string `json:"codec_type"`
}

// progressTail keeps the last timestamp an ffmpeg -progress stream reported.
//
// It holds one duration and at most one partial line, so a decode of any
// length costs the same memory. That is the whole reason it exists: the
// figure it produces decides whether a remux replaces the file it was made
// from, and a buffer the decode can outrun answers with a timestamp from
// early in the file.
type progressTail struct {
	partial  []byte
	elapsed  time.Duration
	reported bool
}

// durationTolerance is how far output duration may drift from its source.
//
// A remux re-derives timestamps, and a transcode's final frame boundary
// need not land exactly, so a small difference is expected. Anything
// larger means content is missing.
const durationTolerance = 2 * time.Second

// discardTimeout bounds cleanup of a worthless file. Cleanup runs on the
// way out of a failure, so it waits briefly for a lock and then leaves the
// file rather than delaying the error that matters.
const discardTimeout = 5 * time.Second

// minRemuxByteRatio is the smallest output-to-source size ratio a stream
// copy may produce.
//
// A remux moves the same elementary streams into a container with less
// per-packet overhead, so it reclaims a few percent and nothing close to
// half. This is the one check on a remux that asks the filesystem instead
// of ffmpeg, so a decoder that stops early and still exits zero cannot
// satisfy it by agreeing with itself.
const minRemuxByteRatio = 0.5

// maxProgressLine bounds one line of an ffmpeg progress stream. Every field
// it writes is a short key=value pair, so anything past this is not a line
// and holding it would defeat the bound the reader exists for.
const maxProgressLine = 4 << 10

// maxCapturedOutput bounds what is kept from a subprocess stream. The text
// is parsed as a short answer or quoted into an error, and a program that
// writes without limit must not be able to size this process.
const maxCapturedOutput = 64 << 10

// maxSilenceWindow bounds the stretch one silence probe is asked about.
//
// A broadcast is hours, so a day is already far past anything real. The
// bound is for the arithmetic that doubles it into a deadline rather than
// for the platform.
const maxSilenceWindow = 24 * time.Hour

// probeTimeout bounds a question about a file's shape. ffprobe reads a
// header and answers, so a probe still running long after that is one that
// is never going to answer.
const probeTimeout = 60 * time.Second

// silenceFloor is the mean level, in dB, at or below which a track holds no
// audible content.
//
// Measured against the platform's own silenced segments, which report about
// -91 dB, where ordinary speech in the same broadcast reports about -25. The
// floor sits far below anything audible and far above the silenced case, so
// a quiet passage is never mistaken for a track that was replaced.
const silenceFloor = -70.0

// silenceDetectPrefix opens every line the silence filter writes, so a
// report is told apart from a line that merely quotes the words.
const silenceDetectPrefix = "[Parsed_silencedetect"

// minSilentRun is the shortest run of quiet the filter reports at all.
//
// Below this every pause between words is a run, and the sum of them across
// a stretch of ordinary speech reaches any fraction worth testing. A
// platform silences in blocks of minutes, so nothing real is missed.
const minSilentRun = 2 * time.Second

// silentEnough is how much of a window has to be silent for the stretch to
// count as silent.
//
// Well under one, because the edges of the window carry ordinary audio: the
// download range is truncated to whole seconds and the platform reports the
// stretch to the second, so a genuinely silenced stretch still shows sound
// at both ends. Well over what speech produces, because a run has to last
// minSilentRun to be counted at all.
const silentEnough = 0.6

// demuxTimeout bounds a pass that reads every packet of a file. A real full
// demux of a four-hour capture takes minutes, so this is a bound only a
// hang can reach, and its job is to end one rather than to limit the work.
const demuxTimeout = 6 * time.Hour

// execCommand builds a subprocess. Tests substitute a helper process so
// the pipeline's behavior is exercised without ffmpeg installed.
var execCommand = exec.CommandContext

// carriedKinds are the stream kinds a replacement must preserve, in the
// order they are mapped into the output.
//
// This is the single statement of what survives a replacement: Remux and
// Transcode map exactly these, and verification requires exactly these. A
// kind missing from the list is a kind the final container cannot hold, so
// the step drops it and the comparison must not ask for it. Timed metadata
// is the only such kind.
var carriedKinds = []streamKind{
	{name: "video", specifier: "v"},
	{name: "audio", specifier: "a"},
	{name: "subtitle", specifier: "s"},
	{name: "attachment", specifier: "t"},
}

// New returns a pipeline backed by the ffmpeg tools.
func New() *Pipeline {
	return &Pipeline{
		ffmpeg:  func() (string, error) { return deps.Path(deps.FFmpeg) },
		ffprobe: func() (string, error) { return deps.Path(deps.FFprobe) },
	}
}

// Error implements error.
func (e *VerifyError) Error() string {
	return fmt.Sprintf("verifying %s against %s: %s", e.Output, e.Source, e.Reason)
}

// ///////////////////////////////////////////////
// Inspection
// ///////////////////////////////////////////////

// SilentBetween reports whether a stretch of a media file carries no
// audible audio.
//
// The stretch is the point. A mean taken over a whole file cannot see a
// short silence inside a long one: ninety seconds of digital silence in a
// fifteen minute range moves the level by less than half a decibel, which
// nothing separates from an ordinary quiet passage. Measured against the
// stretch alone, the same silence is the entire dynamic range.
//
// A stretch holding no audio stream at all is silent by this reading, which
// is the answer a caller wanting audible content needs.
func (p *Pipeline) SilentBetween(ctx context.Context, path string, from, to time.Duration) (bool, error) {
	binary, err := p.ffmpeg()
	if err != nil {
		return false, err
	}
	if to <= from {
		return false, fmt.Errorf("measuring %s: the stretch ends at or before it starts", path)
	}

	// Bounded by the stretch, which is what the seek makes true: an input
	// seek starts the decode at the window, where a filter alone would decode
	// everything before it and a window near the end of a long file would run
	// past this deadline. The floor covers process startup on a machine busy
	// capturing.
	// The span is bounded before it is doubled. It is built from platform
	// mute metadata, and a Duration multiplied past its range wraps
	// negative, which yields a context that has already expired: the probe
	// then fails instantly and reports the window as unreadable rather
	// than as unmeasured.
	window := min(to-from, maxSilenceWindow)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout+2*window)
	defer cancel()

	// -vn so only the audio is decoded. Decoding the video as well answers
	// the same question for many times the work.
	//
	// atrim stays despite the seek, because a seek lands on a keyframe and
	// the window has to be exact. It is stated relative to the seek, so it
	// runs from zero.
	//
	// How much of the window is silent, not how loud it is on average. A
	// mean is a power mean, so a fraction of a second of ordinary audio at
	// either edge lifts it tens of decibels clear of any floor. Measured on
	// a file silenced across a known stretch, the mean over that stretch
	// reads -61 dB where the silence itself is -91, and a second of
	// misalignment takes it to -40. Edge leakage is guaranteed here: the
	// download range is truncated to whole seconds, the seek lands on a
	// frame boundary, and the platform reports the stretch to the second.
	//
	// The report is a log line, so it lands on stderr rather than stdout,
	// and nothing is encoded because the output goes to the null muxer.
	// -nostats keeps the progress meter out of the capture.
	out, err := toolDiagnostics(ctx, binary,
		"-nostats", "-hide_banner",
		"-v", "info",
		"-ss", fmt.Sprintf("%f", from.Seconds()),
		"-i", path,
		"-vn",
		"-af", fmt.Sprintf("atrim=start=0:end=%f,silencedetect=noise=%ddB:d=%f",
			window.Seconds(), int(silenceFloor), minSilentRun.Seconds()),
		"-f", "null", "-")
	if err != nil {
		return false, fmt.Errorf("measuring the audio of %s: %w", path, err)
	}
	return silentFor(out, window) >= time.Duration(float64(window)*silentEnough), nil
}

// silentFor returns how much of a measured window the filter reported as
// silence.
//
// A run still open when the window ends prints no length, because the filter
// closes a run on the sound that ends it and no sound comes. That run is
// counted to the end of the window, which is what a wholly silent stretch
// looks like.
func silentFor(output string, window time.Duration) time.Duration {
	total, openAt, open := time.Duration(0), time.Duration(0), false

	for line := range strings.SplitSeq(output, "\n") {
		if !filterLine(line, silenceDetectPrefix) {
			continue
		}
		if length, found := afterField(line, "silence_duration:"); found {
			total += length
			open = false
			continue
		}
		if at, found := afterField(line, "silence_start:"); found {
			openAt, open = at, true
		}
	}
	if open && window > openAt {
		total += window - openAt
	}
	return min(total, window)
}

// afterField reads the seconds a named field of a filter's report carries.
func afterField(line, field string) (time.Duration, bool) {
	_, after, found := strings.Cut(line, field)
	if !found {
		return 0, false
	}

	text, _, _ := strings.Cut(strings.TrimSpace(after), " ")
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// filterLine reports whether a line is a named filter's own output.
//
// Anchored at the start of the line, and the leading whitespace is not
// trimmed first. The tool prints a file's container metadata before it
// decodes, and a file carrying the filter's own wording in a tag would
// otherwise be read as a report. A real report begins at column zero;
// metadata is indented under the stream it belongs to, so the indentation is
// the whole distinction and removing it removes the check.
func filterLine(line, prefix string) bool {
	return strings.HasPrefix(line, prefix)
}

// Duration returns a media file's container duration.
func (p *Pipeline) Duration(ctx context.Context, path string) (time.Duration, error) {
	binary, err := p.ffprobe()
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := toolOutput(ctx, binary,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path)
	if err != nil {
		return 0, fmt.Errorf("reading duration of %s: %w", path, err)
	}

	text := strings.TrimSpace(out)
	if text == "" || text == "N/A" {
		return 0, fmt.Errorf("reading duration of %s: container reports none", path)
	}

	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing duration %q of %s: %w", text, path, err)
	}
	// ParseFloat accepts nan and inf, and a file stating either has stated
	// nothing. Two files that both fail to state a length would otherwise
	// compare equal and clear the gate that decides a deletion.
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, fmt.Errorf("reading duration of %s: %q is not a length", path, text)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// DemuxClean reads every packet in a file and reports whether any error
// surfaced.
//
// This is the check that catches truncation and corruption that a duration
// comparison alone would miss, because a broken file can still declare a
// plausible duration in its header.
func (p *Pipeline) DemuxClean(ctx context.Context, path string) error {
	binary, err := p.ffmpeg()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, demuxTimeout)
	defer cancel()

	cmd := command(ctx, binary,
		"-hide_banner", "-v", "error",
		"-i", path,
		"-map", "0", "-c", "copy", "-f", "null", "-")

	// ffmpeg reports decode problems on stderr while still exiting zero,
	// so the exit status alone is not enough to call a file clean.
	stderr := procgroup.NewOutput(maxCapturedOutput)
	cmd.Stderr = stderr

	if err := procgroup.Run(cmd); err != nil {
		return fmt.Errorf("demuxing %s: %w: %s", path, err, stderr.Excerpt(procgroup.MaxErrorText))
	}
	if message := stderr.Excerpt(procgroup.MaxErrorText); message != "" {
		return fmt.Errorf("demuxing %s: %s", path, message)
	}
	return nil
}

// command builds a tool invocation.
func command(ctx context.Context, binary string, args ...string) *exec.Cmd {
	return execCommand(ctx, binary, args...)
}

// toolDiagnostics runs a tool and returns what it wrote to standard error.
//
// A measurement ffmpeg reports as a log line reaches that stream and no
// other, so a caller reading stdout sees nothing at all and cannot tell that
// from a measurement of nothing.
//
// The capture keeps the tail. A summary is written after the work, so a
// leading capture discards exactly the part worth reading: a decoder warning
// repeated per frame of a damaged file fills any bound before the answer is
// printed.
func toolDiagnostics(ctx context.Context, binary string, args ...string) (string, error) {
	stdout := procgroup.NewOutput(maxCapturedOutput)
	stderr := procgroup.NewTailOutput(maxCapturedOutput)

	cmd := command(ctx, binary, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := procgroup.Run(cmd); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.Excerpt(procgroup.MaxErrorText))
	}
	return stderr.String(), nil
}

// toolOutput runs a tool and returns its standard output.
//
// Both streams are bounded, because the answer is always short and the
// program producing it is not this one. A bounded answer that ran past the
// bound is refused rather than returned: every caller parses this, and a
// prefix of a JSON inventory parses into a file with fewer streams than it
// has, which is the reading that authorises deleting the original.
func toolOutput(ctx context.Context, binary string, args ...string) (string, error) {
	stdout := procgroup.NewOutput(maxCapturedOutput)
	stderr := procgroup.NewOutput(maxCapturedOutput)

	cmd := command(ctx, binary, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := procgroup.Run(cmd); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.Excerpt(procgroup.MaxErrorText))
	}
	if stdout.Truncated() {
		return "", fmt.Errorf("the answer was truncated at %d bytes", maxCapturedOutput)
	}
	return stdout.String(), nil
}

// ///////////////////////////////////////////////
// Remux
// ///////////////////////////////////////////////

// Remux repackages a capture into its final container without re-encoding.
//
// Twitch's MPEG-TS carries PCR discontinuities at segment seams. VLC drives
// its master clock from PCR, so at the first seam it computes a duration of
// roughly twenty-six hours and stalls. Matroska has no PCR concept, so
// moving the same elementary streams into it yields a correctly timed,
// seekable file. The timed metadata stream is dropped along the way.
//
// The output is a stream copy, so this is lossless. It reclaims only the
// few percent that MPEG-TS packet overhead costs.
func (p *Pipeline) Remux(ctx context.Context, source, output string) error {
	binary, err := p.ffmpeg()
	if err != nil {
		return err
	}
	if err := paths.RequireAbsent("output", output); err != nil {
		return err
	}

	args := []string{
		"-hide_banner", "-v", "error",
		// The capture's timestamps restart at each seam. Regenerating
		// presentation timestamps is what makes the result seekable.
		"-fflags", "+genpts",
		"-i", source,
	}
	args = append(args, mapArgs()...)
	args = append(args, "-c", "copy", "-avoid_negative_ts", "make_zero", output)

	cmd := command(ctx, binary, args...)
	stderr := procgroup.NewOutput(maxCapturedOutput)
	cmd.Stderr = stderr

	if err := procgroup.Run(cmd); err != nil {
		// A partial output would look like a finished remux to the next
		// run, so it goes before the error is returned.
		discard(ctx, output)
		return fmt.Errorf("remuxing %s: %w: %s", source, err, stderr.Excerpt(procgroup.MaxErrorText))
	}
	if err := requireSimilarSize(source, output); err != nil {
		discard(ctx, output)
		return fmt.Errorf("remuxing %s: %w", source, err)
	}
	return nil
}

// mapArgs returns the -map arguments carrying every kind a replacement
// must preserve.
//
// Each is optional, so a capture with no audio, or none with video, is
// replaced rather than refused with "Stream map matches no streams".
func mapArgs() []string {
	args := make([]string, 0, 2*len(carriedKinds))
	for _, kind := range carriedKinds {
		args = append(args, "-map", "0:"+kind.specifier+"?")
	}
	return args
}

// requireSimilarSize rejects a stream copy far smaller than what it copied.
//
// It is the one signal about a remux that does not come from ffmpeg. A
// decoder that stops at a corrupt packet and exits zero reports a
// plausible length for the part it read, so every measurement taken
// through ffmpeg agrees with every other. The bytes on disk do not.
func requireSimilarSize(source, output string) error {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("measuring %s: %w", source, err)
	}
	outputInfo, err := os.Stat(output)
	if err != nil {
		return fmt.Errorf("measuring %s: %w", output, err)
	}
	if sourceInfo.Size() <= 0 {
		return fmt.Errorf("source %s is empty", source)
	}

	if float64(outputInfo.Size())/float64(sourceInfo.Size()) < minRemuxByteRatio {
		return fmt.Errorf("output holds %s copied from %s, too little to be all of it",
			config.Size(outputInfo.Size()), config.Size(sourceInfo.Size()))
	}
	return nil
}

// ///////////////////////////////////////////////
// Verify
// ///////////////////////////////////////////////

// Verify reports whether output faithfully represents source.
//
// It is the gate before a source file may be deleted, so it has to prove
// four separate things. A truncated file can still declare the right
// duration in its header. A file with a wrong duration can still demux
// cleanly. A file that passes both can still be missing a whole track,
// because dropping one changes neither the duration nor the demux. And all
// three are measured against the source, so a source that cannot be read
// whole settles none of them.
func (p *Pipeline) Verify(ctx context.Context, source, output string) error {
	if err := p.verifyStreams(ctx, source, output); err != nil {
		return err
	}
	if err := p.verifyDuration(ctx, source, output); err != nil {
		return err
	}

	// The source is the yardstick, and ffmpeg answers for a file it gave up
	// on with the part of it that read cleanly. A short output then agrees
	// with a short measurement of a long file, and the long file is the one
	// about to be deleted.
	if err := p.DemuxClean(ctx, source); err != nil {
		return &VerifyError{Source: source, Output: output, Reason: err.Error()}
	}

	// The demux is the load-bearing check on the output. It catches
	// truncation that the duration comparison misses entirely, because a
	// truncated container still reports its original header duration.
	if err := p.DemuxClean(ctx, output); err != nil {
		return &VerifyError{Source: source, Output: output, Reason: err.Error()}
	}
	return nil
}

// verifyDuration reports whether output holds as much media as its source.
//
// The container's own figure is checked first because it costs one cheap
// probe. It cannot be trusted on its own: a capture's timestamps restart at
// every segment seam, and a single clock jump makes the source's header
// declare hours that were never recorded. Remux regenerates presentation
// timestamps precisely because of that, so a correct output legitimately
// disagrees with a broken source's header.
//
// Believing the header there would fail every such remux, delete the good
// output, and leave the capture to be remuxed and thrown away again on
// every sweep forever. So a disagreement falls through to decoding the
// source, which is expensive and authoritative, rather than to a verdict.
func (p *Pipeline) verifyDuration(ctx context.Context, source, output string) error {
	declared, err := p.Duration(ctx, source)
	if err != nil {
		return err
	}
	outputDuration, err := p.Duration(ctx, output)
	if err != nil {
		return err
	}
	if within(declared, outputDuration) {
		return nil
	}

	decoded, err := p.decodedDuration(ctx, source)
	if err != nil {
		return err
	}
	if within(decoded, outputDuration) {
		return nil
	}

	return &VerifyError{
		Source: source, Output: output,
		Reason: fmt.Sprintf("output is %s, source decodes to %s (its header claims %s)",
			outputDuration.Round(time.Second), decoded.Round(time.Second), declared.Round(time.Second)),
	}
}

// within reports whether two durations agree inside the tolerance.
func within(a, b time.Duration) bool {
	return time.Duration(math.Abs(float64(a-b))) <= durationTolerance
}

// decodedDuration measures a file by reading it rather than by asking its
// header, regenerating timestamps the way Remux does so both sides of a
// comparison are measured the same way.
func (p *Pipeline) decodedDuration(ctx context.Context, path string) (time.Duration, error) {
	binary, err := p.ffmpeg()
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, demuxTimeout)
	defer cancel()

	args := []string{
		// The machine-readable progress stream rather than the human one.
		// -stats writes a running total into a buffer that has to hold the
		// whole decode, and a long enough decode outruns any buffer: the
		// tail is dropped, the last figure read is from early in the file,
		// and a correct remux is discarded as too short. This form keeps one
		// value, so the memory is bounded by construction rather than by a
		// limit the decode can pass.
		"-hide_banner", "-v", "error", "-nostats", "-progress", "pipe:1",
		"-fflags", "+genpts",
		"-i", path,
	}
	args = append(args, mapArgs()...)
	args = append(args, "-c", "copy", "-f", "null", "-")

	cmd := command(ctx, binary, args...)

	progress := &progressTail{}
	cmd.Stdout = progress
	stderr := procgroup.NewOutput(maxCapturedOutput)
	cmd.Stderr = stderr
	if err := procgroup.Run(cmd); err != nil {
		return 0, fmt.Errorf("decoding %s to measure it: %w: %s", path, err, stderr.Excerpt(procgroup.MaxErrorText))
	}

	// This figure is what authorises deleting the file it measures. A
	// decoder that stopped at a corrupt packet says so and still exits zero,
	// which makes the running total a floor rather than a length.
	if message := nonProgress(stderr.String()); message != "" {
		return 0, fmt.Errorf("decoding %s to measure it: %s", path, message)
	}

	elapsed, ok := progress.last()
	if !ok {
		return 0, fmt.Errorf("decoding %s to measure it: no progress was reported", path)
	}
	return elapsed, nil
}

// Write implements io.Writer, reading whole lines and keeping the newest
// timestamp among them.
func (t *progressTail) Write(chunk []byte) (int, error) {
	written := len(chunk)
	t.partial = append(t.partial, chunk...)

	for {
		end := bytes.IndexByte(t.partial, '\n')
		if end < 0 {
			break
		}
		t.readLine(string(t.partial[:end]))
		t.partial = t.partial[end+1:]
	}

	// A line longer than any ffmpeg writes is not a line. Dropping it keeps
	// the bound this type exists for.
	if len(t.partial) > maxProgressLine {
		t.partial = t.partial[:0]
	}
	return written, nil
}

// readLine takes the timestamp out of one progress field.
//
// out_time_us is microseconds and is what every ffmpeg since 4.0 writes.
// out_time is the clock form, read as the fallback so an older build still
// answers rather than reporting no progress at all.
func (t *progressTail) readLine(line string) {
	field := strings.TrimSpace(line)

	if value, ok := strings.CutPrefix(field, "out_time_us="); ok {
		if micros, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			t.elapsed, t.reported = time.Duration(micros)*time.Microsecond, true
		}
		return
	}
	if value, ok := strings.CutPrefix(field, "out_time="); ok {
		if parsed, err := parseClock(strings.TrimSpace(value)); err == nil {
			t.elapsed, t.reported = parsed, true
		}
	}
}

// last returns the newest timestamp seen, and whether there was one.
func (t *progressTail) last() (time.Duration, bool) {
	return t.elapsed, t.reported
}

// nonProgress returns the first line of ffmpeg output that is not a
// progress report.
//
// Every field of a progress line is a key=value pair, and no diagnostic
// ffmpeg writes is built only from those.
func nonProgress(stderr string) string {
	for line := range strings.SplitSeq(strings.ReplaceAll(stderr, "\r", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for field := range strings.FieldsSeq(trimmed) {
			if !strings.Contains(field, "=") {
				return procgroup.Excerpt(trimmed, 0, procgroup.MaxErrorText)
			}
		}
	}
	return ""
}

// parseClock reads ffmpeg's HH:MM:SS.ss progress format.
func parseClock(value string) (time.Duration, error) {
	// ffmpeg reports a negative offset whenever a stream starts before the
	// container's zero. Dropping the sign doubles the error in a figure
	// compared against a two second tolerance.
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(value, "-")

	hours, rest, ok := strings.Cut(value, ":")
	if !ok {
		return 0, fmt.Errorf("timestamp %q has no hour field", value)
	}
	minutes, seconds, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, fmt.Errorf("timestamp %q has no minute field", value)
	}

	h, err := strconv.Atoi(hours)
	if err != nil {
		return 0, fmt.Errorf("timestamp %q: %w", value, err)
	}
	m, err := strconv.Atoi(minutes)
	if err != nil {
		return 0, fmt.Errorf("timestamp %q: %w", value, err)
	}
	s, err := strconv.ParseFloat(seconds, 64)
	if err != nil {
		return 0, fmt.Errorf("timestamp %q: %w", value, err)
	}
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return 0, fmt.Errorf("timestamp %q has no seconds field", value)
	}

	elapsed := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(s*float64(time.Second))
	if negative {
		return -elapsed, nil
	}
	return elapsed, nil
}

// verifyStreams reports whether output carries every stream the source had.
//
// Replacement selects streams explicitly, so a source with two audio tracks
// could yield an output with one and nothing else would notice: same
// duration, clean demux. The source is then deleted and the second track is
// gone with it.
func (p *Pipeline) verifyStreams(ctx context.Context, source, output string) error {
	sourceStreams, err := p.streamKinds(ctx, source)
	if err != nil {
		return err
	}
	outputStreams, err := p.streamKinds(ctx, output)
	if err != nil {
		return err
	}

	if err := compareStreams(sourceStreams, outputStreams); err != nil {
		return &VerifyError{Source: source, Output: output, Reason: err.Error()}
	}
	return nil
}

// compareStreams reports the first kind of stream the output is short of.
func compareStreams(source, output map[string]int) error {
	for _, kind := range carriedKinds {
		want, got := source[kind.name], output[kind.name]
		if got < want {
			return fmt.Errorf("output has %d %s %s, source has %d",
				got, kind.name, plural(got, "stream", "streams"), want)
		}
	}
	return nil
}

// streamKinds counts a file's streams by kind.
func (p *Pipeline) streamKinds(ctx context.Context, path string) (map[string]int, error) {
	binary, err := p.ffprobe()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// JSON rather than the flat listing, because an MPEG-TS file repeats
	// every stream inside its program and the flat form prints both copies.
	// Counting a capture twice over makes it impossible for any remux of it
	// to carry enough streams.
	out, err := toolOutput(ctx, binary,
		"-v", "error",
		"-show_entries", "stream=codec_type",
		"-of", "json",
		path)
	if err != nil {
		return nil, fmt.Errorf("reading streams of %s: %w", path, err)
	}

	var inventory streamInventory
	if err := json.Unmarshal([]byte(out), &inventory); err != nil {
		return nil, fmt.Errorf("reading streams of %s: %w", path, err)
	}
	// An empty inventory satisfies every comparison it is the source of, so
	// a file that answers with none is refused rather than believed.
	if len(inventory.Streams) == 0 {
		return nil, fmt.Errorf("reading streams of %s: it reports none", path)
	}

	kinds := map[string]int{}
	for _, stream := range inventory.Streams {
		if kind := strings.TrimSpace(stream.CodecType); kind != "" {
			kinds[kind]++
		}
	}
	return kinds, nil
}

// plural picks a word form for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ReplaceVerified runs step, verifies its output against the source,
// records it through commit, and only then removes the source.
//
// commit is what makes the sequence resumable. Recording the output before
// the source goes means a removal that fails leaves the caller's own record
// naming a file that exists and is verified, so the next attempt has
// nothing to redo. Recording it afterwards would leave that record naming
// the source while the finished file sits beside it, and every later
// attempt would refuse to write over that file.
//
// A failed commit discards the output, which puts both the caller's record
// and the filesystem back where the step found them.
//
// The source is kept on every failure, so a failed replacement costs time
// and never content. A failed step cleans up after itself, because only the
// step knows whether the file at the output path is one it wrote.
func (p *Pipeline) ReplaceVerified(ctx context.Context, source, output string, keepSource bool,
	step, commit func() error,
) error {
	if err := refuseSamePath(source, output); err != nil {
		return err
	}
	if err := step(); err != nil {
		return err
	}
	if err := p.Verify(ctx, source, output); err != nil {
		discard(ctx, output)
		return err
	}
	if err := commit(); err != nil {
		discard(ctx, output)
		return err
	}
	if keepSource {
		return nil
	}

	// The output is verified and recorded, so the source is now a duplicate
	// holding space the library is budgeted against. A backup agent reading
	// the capture it just saw finish is the ordinary case, hence the wait.
	if err := fsretry.Remove(ctx, source); err != nil {
		return fmt.Errorf("removing %s, which %s already replaces: %w", source, output, err)
	}
	return nil
}

// refuseSamePath rejects a replacement whose output is its own source.
//
// Every check in Verify compares such a file against itself and passes
// trivially, so this is what stands between a mistaken path and a deletion
// that reports success.
func refuseSamePath(source, output string) error {
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", source, err)
	}
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", output, err)
	}
	if sourceAbs == outputAbs {
		return fmt.Errorf("%s cannot replace itself", source)
	}

	// A case-insensitive volume, a symlink, or a hard link makes two
	// spellings one file, which only the filesystem can answer.
	sourceInfo, sourceErr := os.Stat(sourceAbs)
	outputInfo, outputErr := os.Stat(outputAbs)
	if sourceErr == nil && outputErr == nil && os.SameFile(sourceInfo, outputInfo) {
		return fmt.Errorf("%s and %s are the same file", source, output)
	}
	return nil
}

// discard removes a file whose contents are worthless, reporting nothing.
//
// Every caller is already returning a failure, and a leftover partial file
// is a cosmetic problem beside the one being reported. It still goes
// through fsretry, because a scanner holding the partial file is exactly
// the case a bare os.Remove loses to.
func discard(ctx context.Context, path string) {
	// The cancellation that brought most callers here would skip the
	// cleanup too, so this gets its own short budget.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	_ = fsretry.Remove(ctx, path)
}
