package post

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Helper process
// ///////////////////////////////////////////////

// helperSpecifiers is ffmpeg's stream specifier letter for each kind
// ffprobe names.
//
// It is stated here rather than read from the package so the modelled
// ffmpeg answers to the real tool's spelling. A wrong letter in the
// package under test then shows up as an output missing a stream.
var helperSpecifiers = map[string]string{
	"video":      "v",
	"audio":      "a",
	"subtitle":   "s",
	"attachment": "t",
	"data":       "d",
}

// TestHelperProcess stands in for ffmpeg and ffprobe. It is not a test: it
// runs only when the parent re-invokes this binary.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("helper process, invoked only by fakeExec")
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}

	// A duration disagreement falls back to decoding the source, which
	// reports progress on the pipe it was given rather than a value on
	// stdout.
	if slices.Contains(args, "-progress") {
		reportProgress()
		os.Exit(0)
	}

	// A file that will not read through, named by path so one side of a
	// comparison can be dirty while the other is clean.
	if dirty := os.Getenv("FAKE_DIRTY_PATH"); dirty != "" &&
		slices.Contains(args, dirty) && slices.Contains(args, "null") {
		os.Stderr.WriteString("[mpegts @ 0x1] Packet corrupt, stopping\n")
		os.Exit(0)
	}

	// Selected by mode rather than by arguments, because the handler for
	// the question it floods runs first.
	if os.Getenv("FAKE_MODE") == "flood_stdout" {
		// An answer that runs past the cap. It opens as a valid inventory,
		// so a prefix of it parses into a file with fewer streams than it
		// has, which is the reading that authorises a deletion.
		os.Stdout.WriteString(`{"streams":[{"codec_type":"video"},{"codec_type":"`)
		for range 200 {
			os.Stdout.WriteString(strings.Repeat("padding ", 1024))
		}
		os.Stdout.WriteString(`"}]}`)
		os.Exit(0)
	}

	// Verification asks for the stream inventory before it asks about
	// duration. Answering both from one canned value would let a mode meant
	// to exercise duration report nonsense streams instead.
	if slices.Contains(args, "stream=codec_type") {
		reportStreams(args)
		os.Exit(0)
	}

	switch os.Getenv("FAKE_MODE") {
	case "remux_map":
		// ffprobe and ffmpeg share this mode, because the point of it is
		// that the output is probed as the file the remux arguments would
		// really have produced.
		switch {
		case slices.Contains(args, "format=duration"):
			os.Stdout.WriteString(os.Getenv("FAKE_DURATION") + "\n")
		case slices.Contains(args, "null"):
			// Both files read through cleanly.
		default:
			modelRemux(args)
		}
		os.Exit(0)

	case "flood":
		os.Stderr.WriteString(strings.Repeat("noise ", 200_000))
		os.Exit(0)

	// The silence filter writes its report to stderr as it closes each run,
	// the way the real tool does. Anything reading stdout sees nothing.
	// FAKE_SILENCE is "start:length", and an empty length is a run the
	// window ended before the filter could close.
	case "silence":
		if run := os.Getenv("FAKE_SILENCE"); run != "" {
			start, length, _ := strings.Cut(run, ":")
			os.Stderr.WriteString("[Parsed_silencedetect_1 @ 000001] silence_start: " + start + "\n")
			if length != "" {
				os.Stderr.WriteString(
					"[Parsed_silencedetect_1 @ 000001] silence_end: 0 | silence_duration: " + length + "\n")
			}
		}
		os.Exit(0)

	// A run whose diagnostics fill the bounded capture before the report is
	// written, which is what a damaged file produces.
	case "silence_flood":
		os.Stderr.WriteString(strings.Repeat("[in#0] STSC entry is invalid, skipping\n", 5_000))
		os.Stderr.WriteString("[Parsed_silencedetect_1 @ 000001] silence_start: 0\n")
		os.Stderr.WriteString("[Parsed_silencedetect_1 @ 000001] silence_end: 90 | silence_duration: 90\n")
		os.Exit(0)

	// A container tag carrying the filter's own wording. ffmpeg indents
	// metadata continuation lines, which is what tells them apart.
	case "silence_metadata":
		os.Stderr.WriteString("  [Parsed_silencedetect_1 @ 0] silence_start: 0\n")
		os.Stderr.WriteString("    [Parsed_silencedetect_1 @ 0] silence_duration: 90\n")
		os.Exit(0)

	case "encoders_concurrent":
		if slices.Contains(args, "-encoders") {
			os.Stdout.WriteString(os.Getenv("FAKE_ENCODERS"))
			os.Exit(0)
		}
		// A probe that will not return until the test says so, announcing
		// itself first so the test can see how many are running at once.
		os.WriteFile(filepath.Join(os.Getenv("FAKE_PROBE_DIR"),
			strconv.Itoa(os.Getpid())), []byte("probing"), 0o644)
		for range 500 {
			if _, err := os.Stat(os.Getenv("FAKE_BARRIER")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(0)
	}

	switch os.Getenv("FAKE_MODE") {
	case "duration":
		os.Stdout.WriteString(os.Getenv("FAKE_DURATION") + "\n")
		os.Exit(0)

	case "duration_missing":
		os.Stdout.WriteString("N/A\n")
		os.Exit(0)

	case "duration_garbage":
		os.Stdout.WriteString("about four hours\n")
		os.Exit(0)

	case "duration_pair":
		// The source is probed before the output, so alternate on a marker
		// file to give each call a different answer.
		marker := os.Getenv("FAKE_MARKER")
		if _, err := os.Stat(marker); err == nil {
			os.Stdout.WriteString(os.Getenv("FAKE_DURATION_SECOND") + "\n")
		} else {
			os.WriteFile(marker, []byte("x"), 0o644)
			os.Stdout.WriteString(os.Getenv("FAKE_DURATION_FIRST") + "\n")
		}
		os.Exit(0)

	case "clean":
		os.Exit(0)

	case "dirty":
		// ffmpeg reports decode problems on stderr while exiting zero.
		os.Stderr.WriteString("[matroska @ 0x1] Truncated packet\n")
		os.Exit(0)

	case "fails":
		os.Stderr.WriteString("Invalid data found when processing input\n")
		os.Exit(1)

	case "write_output":
		writeOutputArg(args)
		os.Exit(0)

	case "write_output_then_fail":
		writeOutputArg(args)
		os.Stderr.WriteString("encoder died\n")
		os.Exit(1)

	case "encoders":
		os.Stdout.WriteString(os.Getenv("FAKE_ENCODERS"))
		os.Exit(0)

	case "encoders_selective_probe":
		// Listing succeeds for everything, but encoding fails for the
		// named encoder, which is how a compiled-in encoder behaves on
		// hardware that cannot run it.
		if slices.Contains(args, "-encoders") {
			os.Stdout.WriteString(os.Getenv("FAKE_ENCODERS"))
			os.Exit(0)
		}
		if broken := os.Getenv("FAKE_BROKEN_ENCODER"); broken != "" && slices.Contains(args, broken) {
			os.Stderr.WriteString("No capable devices found\n")
			os.Exit(1)
		}
		os.Exit(0)

	case "record_args":
		os.WriteFile(os.Getenv("FAKE_ARGS_FILE"), []byte(strings.Join(args, "\n")), 0o644)
		os.Exit(0)
	}
	os.Exit(0)
}

// writeOutputArg creates the file named by the last argument, which is
// where ffmpeg puts its output path.
func writeOutputArg(args []string) {
	if len(args) == 0 {
		return
	}
	os.WriteFile(args[len(args)-1], []byte("media"), 0o644)
}

// reportProgress writes the running total a decode reports, in the
// key=value blocks ffmpeg's -progress writes to the pipe it is given.
//
// FAKE_PROGRESS_UPDATES makes it write that many blocks, climbing to the
// final figure, which is what a long decode does. The old bounded buffer
// held about eight hundred of them.
func reportProgress() {
	if clock := os.Getenv("FAKE_DECODED_CLOCK"); clock != "" {
		fmt.Fprintf(os.Stdout, "out_time=%s\nprogress=continue\n", clock)
		os.Stdout.WriteString("progress=end\n")
		return
	}

	seconds := os.Getenv("FAKE_DECODED")
	if seconds == "" {
		seconds = os.Getenv("FAKE_DURATION_FIRST")
	}
	if elapsed, err := strconv.ParseFloat(seconds, 64); err == nil {
		updates := 1
		if count, err := strconv.Atoi(os.Getenv("FAKE_PROGRESS_UPDATES")); err == nil && count > 1 {
			updates = count
		}
		for i := 1; i <= updates; i++ {
			at := elapsed * float64(i) / float64(updates)
			fmt.Fprintf(os.Stdout, "frame=%d\nfps=25.0\nout_time_us=%d\nprogress=continue\n",
				i, int64(at*1e6))
		}
		os.Stdout.WriteString("progress=end\n")
	}

	// A decoder that stops at a corrupt packet says so on its diagnostic
	// stream, and still exits zero.
	if message := os.Getenv("FAKE_DECODE_ERROR"); message != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", message)
	}
}

// reportStreams answers a stream inventory probe for the file it names.
func reportStreams(args []string) {
	path := args[len(args)-1]

	// A remux under test records what its arguments selected, so the output
	// is probed as the file those arguments would really have produced.
	if dir := os.Getenv("FAKE_INVENTORY_DIR"); dir != "" {
		if data, err := os.ReadFile(inventoryPath(dir, path)); err == nil {
			os.Stdout.Write(data)
			return
		}
	}

	inventory := os.Getenv("FAKE_STREAMS")
	if inventory == "" {
		inventory = "video,audio"
	}
	// The source is probed before the output, so a second value lets a
	// test give each file a different inventory.
	if second := os.Getenv("FAKE_STREAMS_SECOND"); second != "" {
		marker := os.Getenv("FAKE_STREAMS_MARKER")
		if _, err := os.Stat(marker); err == nil {
			inventory = second
		} else {
			os.WriteFile(marker, []byte("x"), 0o644)
		}
	}
	if inventory == "none" {
		if slices.Contains(args, "json") {
			os.Stdout.WriteString(inventoryJSON(nil))
		}
		return
	}

	kinds := strings.Split(inventory, ",")
	if slices.Contains(args, "json") {
		os.Stdout.WriteString(inventoryJSON(kinds))
		return
	}

	// ffprobe's flat listing prints an MPEG-TS file's streams twice, once
	// inside its program and once at the top level. A container with no
	// programs is printed once.
	repeats := 1
	if strings.HasSuffix(path, ".ts") {
		repeats = 2
	}
	for range repeats {
		for _, kind := range kinds {
			os.Stdout.WriteString(strings.TrimSpace(kind) + "\n")
		}
	}
}

// modelRemux writes the file ffmpeg would produce from these arguments,
// carrying exactly the stream kinds the -map arguments selected.
func modelRemux(args []string) {
	output := args[len(args)-1]

	var selected []string
	taken := map[string]bool{}
	for kind := range strings.SplitSeq(os.Getenv("FAKE_STREAMS"), ",") {
		specifier := helperSpecifiers[kind]
		switch {
		case specifier == "":
		case slices.Contains(args, "0:"+specifier+"?"):
			selected = append(selected, kind)
		case slices.Contains(args, "0:"+specifier+":0?") && !taken[kind]:
			// An indexed map carries the first stream of its kind only.
			selected = append(selected, kind)
			taken[kind] = true
		}
	}

	os.WriteFile(inventoryPath(os.Getenv("FAKE_INVENTORY_DIR"), output),
		[]byte(inventoryJSON(selected)), 0o644)

	// A stream copy is close to the size of what it copied.
	source, _ := os.ReadFile(os.Getenv("FAKE_SOURCE"))
	os.WriteFile(output, source, 0o644)
}

// inventoryJSON renders ffprobe's stream listing for a set of kinds.
func inventoryJSON(kinds []string) string {
	var out strings.Builder
	out.WriteString(`{"streams":[`)
	for i, kind := range kinds {
		if i > 0 {
			out.WriteString(",")
		}
		fmt.Fprintf(&out, `{"codec_type":%q}`, strings.TrimSpace(kind))
	}
	out.WriteString("]}")
	return out.String()
}

// inventoryPath is where a modelled remux leaves the output's inventory.
func inventoryPath(dir, mediaPath string) string {
	return filepath.Join(dir, filepath.Base(mediaPath)+".streams")
}

// fakeExec redirects pipeline subprocesses to the helper process.
func fakeExec(t *testing.T, mode string, env ...string) {
	t.Helper()

	// The helper is this test binary, so under -cover it carries the same
	// instrumentation. With nowhere to write its profile the runtime prints
	// "GOCOVERDIR not set" on the helper's own stdout, which is the stream
	// the pipeline parses as ffprobe's answer. A directory of its own keeps
	// the warning out of the data.
	coverDir := t.TempDir()

	original := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		helper := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helper...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"FAKE_MODE="+mode,
			"GOCOVERDIR="+coverDir,
		)
		cmd.Env = append(cmd.Env, env...)
		return cmd
	}
	t.Cleanup(func() { execCommand = original })
}

// fakePipeline returns a pipeline whose tool resolution always succeeds.
func fakePipeline() *Pipeline {
	return &Pipeline{
		ffmpeg:  func() (string, error) { return "ffmpeg", nil },
		ffprobe: func() (string, error) { return "ffprobe", nil },
	}
}

// touch creates a file so existence checks see it.
func touch(t *testing.T, path string) string {
	t.Helper()

	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	return path
}

// fill creates a file of a given size, for the checks that weigh one file
// against another.
func fill(t *testing.T, path string, size int) string {
	t.Helper()

	if err := os.WriteFile(path, []byte(strings.Repeat("m", size)), 0o644); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	return path
}

// ///////////////////////////////////////////////
// Duration
// ///////////////////////////////////////////////

func TestSilentBetween_JudgesHowMuchOfTheWindowIsSilent(t *testing.T) {
	// A mean over the window cannot answer this. It is a power mean, so a
	// fraction of a second of ordinary audio at either edge lifts it tens of
	// decibels clear of any floor, and edge leakage is guaranteed: the
	// download range is truncated to whole seconds and the platform reports
	// the silenced stretch to the second. How much of the window is silent
	// is the question that survives that.
	tests := []struct {
		name string
		run  string
		want bool
	}{
		{
			// A silenced stretch with ordinary audio at both edges, which is
			// what one actually looks like once the window is aligned to the
			// platform's own second-resolution bounds.
			name: "silent but for the edges",
			run:  "2:86",
			want: true,
		},
		{
			// The filter closes a run on the sound that ends it, and no
			// sound comes, so the length is never printed.
			name: "silent to the end of the window",
			run:  "1:",
			want: true,
		},
		{
			name: "one pause in ordinary speech",
			run:  "30:4",
			want: false,
		},
		{
			name: "nothing silent at all",
			run:  "",
			want: false,
		},
		{
			// Just under the fraction, so a window that is mostly audible is
			// kept rather than thrown away.
			name: "half silent",
			run:  "0:45",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, "silence", "FAKE_SILENCE="+tt.run)

			silent, err := fakePipeline().SilentBetween(context.Background(), "patch.mp4", 0, 90*time.Second)
			if err != nil {
				t.Fatalf("SilentBetween() err = %v, want nil", err)
			}
			if silent != tt.want {
				t.Errorf("SilentBetween() = %v for a run of %q, want %v", silent, tt.run, tt.want)
			}
		})
	}
}

func TestSilentBetween_IgnoresAContainerTagQuotingTheFilter(t *testing.T) {
	// The file is what the platform served, so its metadata is untrusted. A
	// tag carrying the filter's own wording would otherwise report a whole
	// window of silence and throw away a patch that is fine.
	fakeExec(t, "silence_metadata")

	silent, err := fakePipeline().SilentBetween(context.Background(), "patch.mp4", 0, 90*time.Second)
	if err != nil {
		t.Fatalf("SilentBetween() err = %v, want nil", err)
	}
	if silent {
		t.Error("SilentBetween() = true, want a metadata tag ignored")
	}
}

func TestSilentBetween_MeasuresOnlyTheStretchItWasGiven(t *testing.T) {
	// The whole reason this takes a range, and the reason it seeks: a filter
	// alone decodes everything before the window too, so a stretch near the
	// end of a long file runs past its own deadline.
	args := filepath.Join(t.TempDir(), "args")
	fakeExec(t, "record_args", "FAKE_ARGS_FILE="+args)

	if _, err := fakePipeline().SilentBetween(context.Background(), "patch.mp4",
		2*time.Minute, 3*time.Minute+30*time.Second); err != nil {
		t.Fatalf("SilentBetween() err = %v, want nil", err)
	}

	recorded, err := os.ReadFile(args)
	if err != nil {
		t.Fatalf("reading the recorded arguments: %v", err)
	}
	got := string(recorded)

	if !strings.Contains(got, "-ss\n120.000000") {
		t.Errorf("arguments = %q, want the decode seeked to the window", got)
	}
	// The trim stays despite the seek, because a seek lands on a frame
	// boundary and the window has to be exact. It runs from the seek.
	if !strings.Contains(got, "atrim=start=0:end=90.000000,silencedetect=noise=-70dB") {
		t.Errorf("arguments = %q, want the filter trimmed to the window", got)
	}
	if !strings.Contains(got, "-vn") {
		t.Errorf("arguments = %q, want the video left undecoded", got)
	}
	if !strings.Contains(got, "-nostats") {
		t.Errorf("arguments = %q, want the progress meter out of the capture", got)
	}
}

func TestSilentBetween_RefusesAStretchThatEndsBeforeItStarts(t *testing.T) {
	// A filter given a backwards window measures nothing and answers that
	// nothing is silent, which would pass every check it was asked to make.
	if _, err := fakePipeline().SilentBetween(context.Background(), "patch.mp4",
		2*time.Minute, time.Minute); err == nil {
		t.Error("SilentBetween() err = nil, want a backwards stretch refused")
	}
}

func TestSilentBetween_FindsTheAnswerBehindAFloodOfDiagnostics(t *testing.T) {
	// The report is written as the decode runs, so a capture keeping the
	// opening bytes discards it. A damaged file emits a warning per frame,
	// which fills any bound long before the report appears.
	fakeExec(t, "silence_flood")

	silent, err := fakePipeline().SilentBetween(context.Background(), "patch.mp4", 0, 90*time.Second)
	if err != nil {
		t.Fatalf("SilentBetween() err = %v, want the answer read from the tail", err)
	}
	if !silent {
		t.Error("SilentBetween() = false, want the report behind the flood found")
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		name    string
		seconds string
		want    time.Duration
	}{
		{name: "whole seconds", seconds: "16560", want: 4*time.Hour + 36*time.Minute},
		{name: "fractional", seconds: "1.5", want: 1500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, "duration", "FAKE_DURATION="+tt.seconds)

			got, err := fakePipeline().Duration(context.Background(), "x.mkv")
			if err != nil {
				t.Fatalf("Duration() err = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Duration() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDuration_Rejects(t *testing.T) {
	// ParseFloat accepts nan and inf, and a file stating either has stated
	// nothing. Two files that both fail to state a length compare equal and
	// clear the gate that authorises a deletion.
	tests := []struct {
		name    string
		mode    string
		seconds string
	}{
		{name: "container reports none", mode: "duration_missing"},
		{name: "unparseable value", mode: "duration_garbage"},
		{name: "probe fails", mode: "fails"},
		{name: "not a number", mode: "duration", seconds: "nan"},
		{name: "not a number in capitals", mode: "duration", seconds: "NaN"},
		{name: "infinite", mode: "duration", seconds: "inf"},
		{name: "negative infinite", mode: "duration", seconds: "-Inf"},
		{name: "zero", mode: "duration", seconds: "0"},
		{name: "negative", mode: "duration", seconds: "-60"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, tt.mode, "FAKE_DURATION="+tt.seconds)

			if got, err := fakePipeline().Duration(context.Background(), "x.mkv"); err == nil {
				t.Errorf("Duration() = %s, err = nil, want an error", got)
			}
		})
	}
}

func TestStreamKinds_RefusesATruncatedAnswer(t *testing.T) {
	// A prefix of a stream inventory is a smaller valid inventory. Parsing
	// one reports a file as having fewer streams than it has, and this
	// comparison is the gate that authorises deleting the original.
	fakeExec(t, "flood_stdout")

	got, err := fakePipeline().streamKinds(context.Background(), "x.mkv")
	if err == nil {
		t.Fatalf("streamKinds() = %v with err = nil, want a truncated answer refused", got)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("streamKinds() err = %v, want it to name truncation", err)
	}
}

func TestDuration_BoundsTheProbe(t *testing.T) {
	// A probe with no deadline of its own inherits whatever the caller had,
	// and a sweep runs with none at all.
	fakeExec(t, "duration", "FAKE_DURATION=60")

	var deadline time.Time
	fake := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		deadline, _ = ctx.Deadline()
		return fake(ctx, name, args...)
	}

	if _, err := fakePipeline().Duration(context.Background(), "x.mkv"); err != nil {
		t.Fatalf("Duration() err = %v, want nil", err)
	}
	if deadline.IsZero() {
		t.Fatal("the probe ran with no deadline, so a hung ffprobe never ends")
	}
	if remaining := time.Until(deadline); remaining > probeTimeout {
		t.Errorf("the probe has %s left, want no more than %s", remaining, probeTimeout)
	}
}

// ///////////////////////////////////////////////
// DemuxClean
// ///////////////////////////////////////////////

func TestDemuxClean_BoundsTheDemux(t *testing.T) {
	// A full demux of a four-hour capture takes minutes, so this bound is
	// only ever reached by a hang. Without it a hung ffmpeg holds the sweep
	// forever.
	fakeExec(t, "clean")

	var deadline time.Time
	fake := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		deadline, _ = ctx.Deadline()
		return fake(ctx, name, args...)
	}

	if err := fakePipeline().DemuxClean(context.Background(), "x.mkv"); err != nil {
		t.Fatalf("DemuxClean() err = %v, want nil", err)
	}
	if deadline.IsZero() {
		t.Fatal("the demux ran with no deadline, so a hung ffmpeg never ends")
	}
	if remaining := time.Until(deadline); remaining > demuxTimeout {
		t.Errorf("the demux has %s left, want no more than %s", remaining, demuxTimeout)
	}
}

func TestDemuxClean(t *testing.T) {
	fakeExec(t, "clean")

	if err := fakePipeline().DemuxClean(context.Background(), "x.mkv"); err != nil {
		t.Errorf("DemuxClean() err = %v, want nil", err)
	}
}

func TestDemuxClean_StderrMeansDirtyEvenOnExitZero(t *testing.T) {
	// ffmpeg reports decode problems on stderr while still exiting zero.
	// Trusting the exit status alone would pass a truncated file and let
	// the source be deleted.
	fakeExec(t, "dirty")

	err := fakePipeline().DemuxClean(context.Background(), "x.mkv")
	if err == nil {
		t.Fatal("DemuxClean() err = nil, want the stderr output treated as a failure")
	}
	if !strings.Contains(err.Error(), "Truncated packet") {
		t.Errorf("DemuxClean() err = %q, want it to carry the ffmpeg message", err)
	}
}

func TestDemuxClean_NonZeroExit(t *testing.T) {
	fakeExec(t, "fails")

	if err := fakePipeline().DemuxClean(context.Background(), "x.mkv"); err == nil {
		t.Error("DemuxClean() err = nil, want an error")
	}
}

// ///////////////////////////////////////////////
// Remux
// ///////////////////////////////////////////////

func TestRemux(t *testing.T) {
	fakeExec(t, "write_output")
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := filepath.Join(dir, "final.mkv")

	if err := fakePipeline().Remux(context.Background(), source, output); err != nil {
		t.Fatalf("Remux() err = %v, want nil", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Errorf("remux output missing: %v", err)
	}
}

func TestRemux_RefusesToOverwrite(t *testing.T) {
	fakeExec(t, "write_output")
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	err := fakePipeline().Remux(context.Background(), source, output)
	if err == nil {
		t.Fatal("Remux() err = nil, want a refusal to overwrite an existing file")
	}
	// The path is the whole diagnostic: the file it refused is a library
	// file, and an operator has no other way to learn which one.
	if !strings.Contains(err.Error(), output) {
		t.Errorf("Remux() err = %q, want it to name the occupied path", err)
	}
}

func TestRemux_RefusesAnOutputTooSmallToBeACopy(t *testing.T) {
	// A stream copy reclaims the few percent MPEG-TS packet overhead costs.
	// A decoder that stopped at a corrupt packet reports a plausible length
	// for the part it read, so every measurement taken through ffmpeg
	// agrees with every other one. The bytes on disk are the only thing
	// that does not.
	fakeExec(t, "write_output")
	dir := t.TempDir()
	source := fill(t, filepath.Join(dir, "capture.ts"), 8192)
	output := filepath.Join(dir, "final.mkv")

	err := fakePipeline().Remux(context.Background(), source, output)
	if err == nil {
		t.Fatal("Remux() err = nil, want an output far smaller than its source refused")
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat output = %v, want the short output removed", statErr)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Errorf("stat source = %v, want the source kept", statErr)
	}
}

func TestRemux_RemovesAPartialOutputOnFailure(t *testing.T) {
	// A half-written file left behind would look like a finished remux to
	// the next run.
	fakeExec(t, "write_output_then_fail")
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := filepath.Join(dir, "final.mkv")

	if err := fakePipeline().Remux(context.Background(), source, output); err == nil {
		t.Fatal("Remux() err = nil, want the failure surfaced")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat output = %v, want the partial file removed", err)
	}
}

// ///////////////////////////////////////////////
// Verify
// ///////////////////////////////////////////////

func TestVerify_Passes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=16560",
		"FAKE_DURATION_SECOND=16560")

	// DemuxClean shares the same helper mode, which exits zero and writes
	// nothing to stderr, so the demux reads as clean.
	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err != nil {
		t.Errorf("Verify() err = %v, want nil", err)
	}
}

func TestVerify_ToleratesSmallDrift(t *testing.T) {
	// A remux regenerates timestamps, so an exact match is not expected.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=16560",
		"FAKE_DURATION_SECOND=16561")

	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err != nil {
		t.Errorf("Verify() err = %v, want nil for one second of drift", err)
	}
}

func TestVerify_RejectsMissingContent(t *testing.T) {
	// A recording that lost an hour still declares a plausible duration in
	// its own header. Comparing against the source is what catches it.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=16560",
		"FAKE_DURATION_SECOND=12960")

	err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv")

	var verifyErr *VerifyError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("Verify() err = %v, want a *VerifyError", err)
	}
	// The reason has to carry both figures, because the operator's next
	// question is always how much is missing.
	for _, want := range []string{"3h36m0s", "4h36m0s"} {
		if !strings.Contains(verifyErr.Reason, want) {
			t.Errorf("VerifyError.Reason = %q, want it to contain %q", verifyErr.Reason, want)
		}
	}
}

func TestVerify_TrustsADecodedSourceOverABrokenHeader(t *testing.T) {
	// A capture's timestamps restart at every segment seam, and one clock
	// jump makes the container declare hours that were never recorded. That
	// is the ordinary Twitch capture. Believing the header would fail every
	// such remux, delete the good output, and leave the capture to be
	// remuxed and thrown away again on every sweep.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		// The source's header claims almost a full day for ten seconds of
		// media, which is what a real discontinuity produces.
		"FAKE_DURATION_FIRST=86005",
		"FAKE_DURATION_SECOND=10",
		"FAKE_DECODED=10")

	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err != nil {
		t.Errorf("Verify() err = %v, want the decoded source to settle it", err)
	}
}

func TestVerify_MeasuresALongDecode(t *testing.T) {
	// A Twitch capture's header never agrees with a correct output, so
	// falling through to the decode is the routine path rather than an
	// exotic one. ffmpeg writes a progress update about twice a second, so a
	// decode running longer than a few minutes writes more updates than any
	// fixed buffer holds. A reader that kept the first bytes and dropped the
	// tail answers with a timestamp from early in the file, the durations
	// disagree, the correct remux is discarded, and three sweeps later the
	// recording is abandoned.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=86005",
		"FAKE_DURATION_SECOND=21600",
		"FAKE_DECODED=21600",
		// Far more than the sixty-four kilobytes the old buffer held.
		"FAKE_PROGRESS_UPDATES=20000")

	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err != nil {
		t.Errorf("Verify() err = %v, want the whole decode measured", err)
	}
}

func TestVerify_RejectsAnOutputMissingATrack(t *testing.T) {
	// Through Verify rather than through compareStreams, because the defect
	// worth catching is verification not asking at all. Dropping a track
	// changes neither the duration nor the demux, so the source would be
	// deleted with the only copy of that track still in it.
	dir := t.TempDir()
	fakeExec(t, "duration",
		"FAKE_DURATION=16560",
		"FAKE_STREAMS_MARKER="+filepath.Join(dir, "streams"),
		"FAKE_STREAMS=video,audio,audio",
		"FAKE_STREAMS_SECOND=video,audio")

	err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv")

	var verifyErr *VerifyError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("Verify() err = %v, want a *VerifyError for the lost audio track", err)
	}
	if !strings.Contains(verifyErr.Reason, "audio") {
		t.Errorf("VerifyError.Reason = %q, want it to name the missing kind", verifyErr.Reason)
	}
}

func TestVerify_RefusesASourceThatStoppedDecoding(t *testing.T) {
	// A six hour capture whose header is right and whose middle is corrupt.
	// ffmpeg stops at the bad packet, reports the thirty minutes it
	// managed, and exits zero. The remux of those thirty minutes is
	// genuinely clean, so believing the measurement deletes five and a half
	// hours of broadcast.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=21600",
		"FAKE_DURATION_SECOND=1800",
		"FAKE_DECODED=1800",
		"FAKE_DECODE_ERROR=[mpegts @ 0x1] Packet corrupt, stopping.")

	err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv")
	if err == nil {
		t.Fatal("Verify() err = nil, want the source's own decode failure to block the comparison")
	}
	if !strings.Contains(err.Error(), "Packet corrupt") {
		t.Errorf("Verify() err = %q, want it to carry what ffmpeg reported", err)
	}
}

func TestVerify_RefusesASourceThatDoesNotReadThrough(t *testing.T) {
	// The durations agree here, so nothing else in verification looks at
	// the source again. A source that cannot be read whole is not a
	// yardstick, whatever its header and its remux agree on.
	fakeExec(t, "duration",
		"FAKE_DURATION=16560",
		"FAKE_DIRTY_PATH=a.ts")

	err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv")

	var verifyErr *VerifyError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("Verify() err = %v, want a *VerifyError for an unreadable source", err)
	}
	if !strings.Contains(verifyErr.Reason, "a.ts") {
		t.Errorf("VerifyError.Reason = %q, want it to name the source", verifyErr.Reason)
	}
}

func TestVerify_AcceptsATruncatedSource(t *testing.T) {
	// MPEG-TS is the capture container precisely because a machine that
	// reboots mid-broadcast leaves a file that still plays. ffmpeg reads a
	// truncated capture through without complaint, and refusing one would
	// leave every interrupted recording out of the library.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=100",
		"FAKE_DURATION_SECOND=100")

	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err != nil {
		t.Errorf("Verify() err = %v, want a capture that reads through to verify", err)
	}
}

func TestVerify_KeepsTheSignOfADecodedMeasurement(t *testing.T) {
	// ffmpeg reports a negative offset whenever a stream starts before the
	// container's zero. Dropping the sign turns thirty seconds into sixty
	// against a two second tolerance.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=16560",
		"FAKE_DURATION_SECOND=30",
		"FAKE_DECODED_CLOCK=-00:00:30.00")

	err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv")

	var verifyErr *VerifyError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("Verify() err = %v, want a *VerifyError", err)
	}
	if !strings.Contains(verifyErr.Reason, "-30s") {
		t.Errorf("VerifyError.Reason = %q, want the decoded measurement to keep its sign", verifyErr.Reason)
	}
}

func TestVerify_RefusesAFileThatReportsNoStreams(t *testing.T) {
	// An empty inventory satisfies every comparison it is the source of, so
	// an output holding one video stream verifies against a source that
	// answered with nothing, and the source is deleted.
	fakeExec(t, "duration",
		"FAKE_DURATION=16560",
		"FAKE_STREAMS=none",
		"FAKE_STREAMS_MARKER="+filepath.Join(t.TempDir(), "streams"),
		"FAKE_STREAMS_SECOND=video")

	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err == nil {
		t.Error("Verify() err = nil, want a source that reports no streams refused")
	}
}

func TestVerify_CountsACaptureStreamOnce(t *testing.T) {
	// ffprobe's flat listing prints an MPEG-TS file's streams twice, once
	// inside its program and once at the top level. Counting both makes
	// every capture look like it carried twice what it did, so no remux of
	// one can ever carry enough and none of them verifies.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=16560",
		"FAKE_DURATION_SECOND=16560",
		"FAKE_STREAMS=video,audio")

	if err := fakePipeline().Verify(context.Background(), "a.ts", "b.mkv"); err != nil {
		t.Errorf("Verify() err = %v, want a capture's streams counted once", err)
	}
}

func TestDemuxClean_BoundsWhatItQuotes(t *testing.T) {
	// The text is attacker-influenced and reaches a log line and a
	// notification body, so the error carries a bounded quote of it.
	fakeExec(t, "flood")

	err := fakePipeline().DemuxClean(context.Background(), "x.mkv")
	if err == nil {
		t.Fatal("DemuxClean() err = nil, want the output treated as a failure")
	}
	if len(err.Error()) > 4*procgroup.MaxErrorText {
		t.Errorf("DemuxClean() err is %d bytes, want it bounded near %d", len(err.Error()), procgroup.MaxErrorText)
	}
	if !strings.Contains(err.Error(), "more bytes") {
		t.Errorf("DemuxClean() err = %q, want it to say output was left out", err)
	}
}

func TestCompareStreams(t *testing.T) {
	// Dropping a track changes neither the duration nor the demux, so
	// nothing else catches it, and the source is deleted straight after.
	tests := []struct {
		name    string
		source  map[string]int
		output  map[string]int
		wantErr bool
	}{
		{
			name:   "identical",
			source: map[string]int{"video": 1, "audio": 1},
			output: map[string]int{"video": 1, "audio": 1},
		},
		{
			// Ordinary on a YouTube multi-language broadcast.
			name:    "a lost second audio track",
			source:  map[string]int{"video": 1, "audio": 2},
			output:  map[string]int{"video": 1, "audio": 1},
			wantErr: true,
		},
		{
			name:    "a lost video track",
			source:  map[string]int{"video": 1, "audio": 1},
			output:  map[string]int{"audio": 1},
			wantErr: true,
		},
		{
			// The remux drops timed metadata on purpose.
			name:   "dropped timed metadata is fine",
			source: map[string]int{"video": 1, "audio": 1, "data": 1},
			output: map[string]int{"video": 1, "audio": 1},
		},
		{
			// A capture with burned-in captions carries a subtitle stream,
			// and losing it changes neither duration nor demux.
			name:    "a lost subtitle stream",
			source:  map[string]int{"video": 1, "audio": 1, "subtitle": 1},
			output:  map[string]int{"video": 1, "audio": 1},
			wantErr: true,
		},
		{
			name:   "an extra track is not a loss",
			source: map[string]int{"video": 1, "audio": 1},
			output: map[string]int{"video": 1, "audio": 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := compareStreams(tt.source, tt.output)
			if tt.wantErr && err == nil {
				t.Error("compareStreams() err = nil, want the lost track reported")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("compareStreams() err = %v, want nil", err)
			}
		})
	}
}

// ///////////////////////////////////////////////
// ReplaceVerified
// ///////////////////////////////////////////////

func TestReplaceVerified_RemovesTheSourceOnlyAfterVerifying(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=100",
		"FAKE_DURATION_SECOND=100")

	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	if err := fakePipeline().ReplaceVerified(context.Background(), source, output, false,
		func() error { return nil },
		func() error { return nil }); err != nil {
		t.Fatalf("ReplaceVerified() err = %v, want nil", err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat source = %v, want it removed after verification", err)
	}
}

func TestReplaceVerified_KeepsTheSourceWhenVerificationFails(t *testing.T) {
	// This is the whole point of the gate: a failed step costs time, never
	// content.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=16560",
		"FAKE_DURATION_SECOND=60")

	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	if err := fakePipeline().ReplaceVerified(context.Background(), source, output, false,
		func() error { return nil },
		func() error { return nil }); err == nil {
		t.Fatal("ReplaceVerified() err = nil, want the verification failure surfaced")
	}
	if _, err := os.Stat(source); err != nil {
		t.Errorf("stat source = %v, want the source kept", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat output = %v, want the unverified output removed", err)
	}
}

func TestReplaceVerified_KeepsTheSourceWhenAsked(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=100",
		"FAKE_DURATION_SECOND=100")

	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	if err := fakePipeline().ReplaceVerified(context.Background(), source, output, true,
		func() error { return nil },
		func() error { return nil }); err != nil {
		t.Fatalf("ReplaceVerified() err = %v, want nil", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Errorf("stat source = %v, want it kept", err)
	}
}

func TestReplaceVerified_PropagatesAStepFailure(t *testing.T) {
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))

	wantErr := errors.New("encoder exploded")
	err := fakePipeline().ReplaceVerified(context.Background(), source,
		filepath.Join(dir, "final.mkv"), false,
		func() error { return wantErr },
		func() error { return nil })

	if !errors.Is(err, wantErr) {
		t.Errorf("ReplaceVerified() err = %v, want the step's error", err)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Errorf("stat source = %v, want it kept", statErr)
	}
}

func TestReplaceVerified_RecordsTheOutputBeforeRemovingTheSource(t *testing.T) {
	// A removal that fails after the output is recorded leaves a caller
	// naming a file that exists and is verified. Recording it afterwards
	// leaves the caller naming the source with the finished file beside it,
	// and every later attempt refuses to write over that file.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=100",
		"FAKE_DURATION_SECOND=100")

	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	var sourcePresentAtCommit bool
	var committed bool
	err := fakePipeline().ReplaceVerified(context.Background(), source, output, false,
		func() error { return nil },
		func() error {
			committed = true
			_, statErr := os.Stat(source)
			sourcePresentAtCommit = statErr == nil
			return nil
		})
	if err != nil {
		t.Fatalf("ReplaceVerified() err = %v, want nil", err)
	}

	if !committed {
		t.Fatal("commit never ran, so nothing recorded the verified output")
	}
	if !sourcePresentAtCommit {
		t.Error("the source was already gone when the output was recorded")
	}
}

func TestReplaceVerified_KeepsTheRecordedOutputWhenTheSourceCannotBeRemoved(t *testing.T) {
	// A backup agent holding the capture past the retry window is the case
	// the retry exists for. What must not happen is the caller being left
	// naming a file that is not the recording.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=100",
		"FAKE_DURATION_SECOND=100")

	dir := t.TempDir()
	// A directory with something in it refuses to be removed on every
	// platform, which is what a held file amounts to here.
	source := filepath.Join(dir, "capture.ts")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("creating %s: %v", source, err)
	}
	touch(t, filepath.Join(source, "held"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	var recorded string
	err := fakePipeline().ReplaceVerified(context.Background(), source, output, false,
		func() error { return nil },
		func() error { recorded = output; return nil })

	if err == nil {
		t.Fatal("ReplaceVerified() err = nil, want the failed removal surfaced")
	}
	if recorded != output {
		t.Errorf("recorded = %q, want the verified output recorded before the removal was attempted", recorded)
	}
	if _, statErr := os.Stat(output); statErr != nil {
		t.Errorf("stat output = %v, want the verified output kept", statErr)
	}
}

func TestReplaceVerified_PutsEverythingBackWhenTheCommitFails(t *testing.T) {
	// The caller's record and the filesystem have to move together. An
	// output left on disk that nothing names is a file every later attempt
	// refuses to write over.
	marker := filepath.Join(t.TempDir(), "marker")
	fakeExec(t, "duration_pair",
		"FAKE_MARKER="+marker,
		"FAKE_DURATION_FIRST=100",
		"FAKE_DURATION_SECOND=100")

	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))
	output := touch(t, filepath.Join(dir, "final.mkv"))

	wantErr := errors.New("database is locked")
	err := fakePipeline().ReplaceVerified(context.Background(), source, output, false,
		func() error { return nil },
		func() error { return wantErr })

	if !errors.Is(err, wantErr) {
		t.Fatalf("ReplaceVerified() err = %v, want the commit failure", err)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Errorf("stat source = %v, want the source kept", statErr)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat output = %v, want the unrecorded output removed", statErr)
	}
}

func TestReplaceVerified_RefusesAFileAsItsOwnReplacement(t *testing.T) {
	// Every check compares such a file against itself and passes trivially,
	// so the file is deleted and the call reports success.
	dir := t.TempDir()
	source := touch(t, filepath.Join(dir, "capture.ts"))

	tests := []struct {
		name   string
		output string
	}{
		{name: "the same path", output: source},
		{name: "the same path spelled differently", output: filepath.Join(dir, ".", "capture.ts")},
		{name: "a relative spelling of the same path", output: filepath.Join(dir, "sub", "..", "capture.ts")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, "clean")

			err := fakePipeline().ReplaceVerified(context.Background(), source, tt.output, false,
				func() error { return nil },
				func() error { return nil })
			if err == nil {
				t.Fatal("ReplaceVerified() err = nil, want a refusal to replace a file with itself")
			}
			if _, statErr := os.Stat(source); statErr != nil {
				t.Errorf("stat source = %v, want the file untouched", statErr)
			}
		})
	}
}

func TestReplaceVerified_AcceptsACaptureWithMoreThanOneAudioTrack(t *testing.T) {
	// Mapping one video and one audio while verification demands every kind
	// the source carried makes a multi-audio capture impossible to verify.
	// awaiting_finalize is swept on a timer with no attempt counter, so one
	// such capture makes the sweeper remux a multi-gigabyte file and throw
	// the result away on every pass, forever.
	dir := t.TempDir()
	source := fill(t, filepath.Join(dir, "capture.ts"), 4096)
	output := filepath.Join(dir, "final.mkv")

	fakeExec(t, "remux_map",
		"FAKE_DURATION=16560",
		"FAKE_SOURCE="+source,
		"FAKE_STREAMS=video,audio,audio,subtitle,data",
		"FAKE_INVENTORY_DIR="+t.TempDir())

	pipeline := fakePipeline()
	err := pipeline.ReplaceVerified(context.Background(), source, output, false,
		func() error { return pipeline.Remux(context.Background(), source, output) },
		func() error { return nil })
	if err != nil {
		t.Fatalf("ReplaceVerified() err = %v, want a multi-audio capture to verify", err)
	}

	if _, statErr := os.Stat(source); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat source = %v, want it removed once the remux verified", statErr)
	}
	if _, statErr := os.Stat(output); statErr != nil {
		t.Errorf("stat output = %v, want the remux kept", statErr)
	}
}
