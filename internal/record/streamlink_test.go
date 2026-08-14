package record

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Helper process
// ///////////////////////////////////////////////

// sentinelToken stands in for a credential in the argv assertions below.
const sentinelToken = "EXAMPLETOKENEXAMPLETOKEN123456"

// TestHelperProcess stands in for the streamlink executable. It is not a
// test: it runs only when the parent re-invokes this binary, and emulates
// the behavior FAKE_MODE selects.
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

	switch os.Getenv("FAKE_MODE") {
	case "live":
		os.Stdout.WriteString(`{"plugin":"twitch",` +
			`"metadata":{"id":"51234","author":"ExampleChannel","category":"Just Chatting",` +
			`"title":"Midnight Build Stream"},` +
			`"streams":{"1080p60":{},"720p":{},"best":{},"worst":{}}}`)
		os.Exit(0)

	case "live_with_warning":
		// streamlink can print a warning ahead of its JSON body.
		os.Stdout.WriteString("[cli][warning] something happened\n" +
			`{"plugin":"twitch","metadata":{"id":"1"},"streams":{"best":{}}}`)
		os.Exit(0)

	case "live_with_braced_warning":
		// A warning carrying a brace of its own, which is not where the
		// response begins.
		os.Stdout.WriteString("[cli][warning] cannot parse {timestamp} in manifest\n" +
			`{"plugin":"twitch","metadata":{"id":"1"},"streams":{"best":{}}}`)
		os.Exit(0)

	case "flooded_error":
		fmt.Fprintf(os.Stdout, `{"error":%q}`, "Unauthorized: "+strings.Repeat("padding ", 200_000))
		os.Exit(1)

	case "record_args":
		os.WriteFile(os.Getenv("FAKE_ARGS_FILE"), []byte(strings.Join(args, "\n")), 0o644)
		os.Exit(0)

	case "live_no_metadata":
		os.Stdout.WriteString(`{"plugin":"twitch","metadata":{},"streams":{"best":{}}}`)
		os.Exit(0)

	case "offline":
		// A body carrying the plugin field. streamlink 8.5.0 does not send
		// one on a failure, so this stands for a future version that does
		// rather than for anything observed.
		os.Stdout.WriteString(`{"plugin":"twitch",` +
			`"error":"No playable streams found on this URL: twitch.tv/examplechannel"}`)
		os.Exit(1)

	case "offline_no_plugin":
		// What a quiet channel really answers. Captured from streamlink
		// 8.5.0 against twitch.tv/atrioc: the error field and nothing else.
		os.Stdout.WriteString(`{"error":"No playable streams found on this URL: twitch.tv/examplechannel"}`)
		os.Exit(1)

	case "offline_phrase_mid_string":
		// A broadcast title quoted back inside a real failure. Searching
		// the whole field for the phrase takes a live channel off the air.
		os.Stdout.WriteString(`{"plugin":"twitch",` +
			`"error":"Unauthorized while opening \"No playable streams found\" (2019 rerun)"}`)
		os.Exit(1)

	case "offline_with_streams":
		// A response contradicting itself. The reading that stops a
		// recording must not be the one that wins.
		os.Stdout.WriteString(`{"plugin":"twitch",` +
			`"error":"No playable streams found on this URL: twitch.tv/examplechannel",` +
			`"streams":{"best":{},"720p":{}}}`)
		os.Exit(1)

	case "flooded_streams":
		// A body that runs past the cap. It opens as a live response, so a
		// prefix of it parses cleanly into a channel with no streams.
		os.Stdout.WriteString(`{"plugin":"twitch","metadata":{"id":"1","title":"`)
		for range 600 {
			os.Stdout.WriteString(strings.Repeat("padding ", 1024))
		}
		os.Stdout.WriteString(`"},"streams":{"best":{}}}`)
		os.Exit(0)

	case "no_streams":
		os.Stdout.WriteString(`{"plugin":"twitch","metadata":{},"streams":{}}`)
		os.Exit(0)

	case "auth_error":
		os.Stdout.WriteString(`{"error":"Unauthorized: token expired"}`)
		os.Exit(1)

	case "garbage":
		os.Stdout.WriteString("not json at all")
		os.Exit(1)

	case "capture":
		writeOutput(args, strings.Repeat("v", 4096))
		os.Exit(0)

	case "capture_partial":
		// A dropped connection leaves bytes on disk and a non-zero exit.
		writeOutput(args, strings.Repeat("v", 512))
		os.Exit(130)

	case "capture_muxer":
		// streamlink starts ffmpeg to mux separate video and audio, so the
		// process writing the recording is not the one the daemon started.
		muxer := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--") // G204: the only program started is this test binary
		muxer.Env = append(os.Environ(), "FAKE_MODE=capture_muxer_child",
			"FAKE_OUTPUT="+outputArg(args))
		if err := muxer.Start(); err != nil {
			os.Exit(1)
		}
		os.WriteFile(os.Getenv("FAKE_STARTED"), []byte("running"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "capture_muxer_child":
		// Appends to the recording, exactly as an abandoned muxer does.
		for range 300 {
			file, err := os.OpenFile(os.Getenv("FAKE_OUTPUT"),
				os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				os.Exit(1)
			}
			file.WriteString("v")
			file.Close()
			time.Sleep(100 * time.Millisecond)
		}
		os.Exit(0)

	case "capture_hang":
		writeOutput(args, "v")
		// Announced after the bytes are on disk, so a test that waits for
		// this knows the capture already has something to count and can
		// cancel a process that is certainly running.
		os.WriteFile(os.Getenv("FAKE_STARTED"), []byte("running"), 0o644)
		time.Sleep(30 * time.Second)
		// Only reached by running the work out to the end, which a
		// cancelled capture never does.
		os.WriteFile(os.Getenv("FAKE_FINISHED"), []byte("ran out"), 0o644)
		os.Exit(0)
	}
	os.Exit(0)
}

// writeOutput writes content to the path following --output.
func writeOutput(args []string, content string) {
	if path := outputArg(args); path != "" {
		os.WriteFile(path, []byte(content), 0o644)
	}
}

// outputArg returns the path following --output.
func outputArg(args []string) string {
	for i, arg := range args {
		if arg == "--output" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// fakeExec redirects engine invocations to the helper process.
func fakeExec(t *testing.T, mode string, env ...string) {
	t.Helper()

	// The helper is this test binary, so under -cover it carries the same
	// instrumentation. Given nowhere to write its profile, the runtime
	// prints a warning on the helper's own streams, which is the stream
	// this package parses a probe answer out of.
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

// fakeEngine returns an engine whose executable resolution always succeeds.
func fakeEngine() *Streamlink {
	return &Streamlink{
		resolve: func() (string, error) { return "streamlink", nil },
	}
}

// ///////////////////////////////////////////////
// Probe
// ///////////////////////////////////////////////

func TestProbe_Live(t *testing.T) {
	fakeExec(t, "live")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err != nil {
		t.Fatalf("Probe() err = %v, want nil", err)
	}
	if !got.Live {
		t.Error("Live = false, want true")
	}
	if got.Metadata.Author != "ExampleChannel" {
		t.Errorf("Author = %q, want %q", got.Metadata.Author, "ExampleChannel")
	}
	if got.Metadata.Title != "Midnight Build Stream" {
		t.Errorf("Title = %q, want the broadcast title", got.Metadata.Title)
	}
	if got.Metadata.ID != "51234" {
		t.Errorf("ID = %q, want %q", got.Metadata.ID, "51234")
	}

	want := []string{"1080p60", "720p", "best", "worst"}
	if len(got.Qualities) != len(want) {
		t.Fatalf("Qualities = %v, want %v", got.Qualities, want)
	}
	for i, quality := range want {
		if got.Qualities[i] != quality {
			t.Errorf("Qualities[%d] = %q, want %q (sorted)", i, got.Qualities[i], quality)
		}
	}
}

func TestProbe_Offline(t *testing.T) {
	// An offline channel is the normal case, not a failure. Returning an
	// error here would make the poll loop log a fault every interval for
	// every channel that is not currently live.
	fakeExec(t, "offline")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err != nil {
		t.Fatalf("Probe() err = %v, want nil for an offline channel", err)
	}
	if got.Live {
		t.Error("Live = true, want false")
	}
}

func TestProbe_ClassifiesOfflineOnlyOnTheWholeSignal(t *testing.T) {
	// streamlink reports an offline channel and a broken one through the
	// same exit code and the same free-text field, so the text is all there
	// is. Misreading it either way is expensive: a live channel called
	// offline is a broadcast silently missed, and an offline channel called
	// an error backs the poll off and notifies about a channel that is
	// merely quiet.
	tests := []struct {
		name     string
		mode     string
		wantLive bool
		wantErr  bool
	}{
		{
			name: "the marker opens the error and a plugin resolved",
			mode: "offline",
		},
		{
			// The body a real offline channel produces. streamlink 8.5.0
			// answers twitch.tv/<channel> with the error and nothing else,
			// no plugin field, so requiring one makes every quiet channel
			// an error: the poll backs off to its ceiling, the daemon joins
			// late, and no broadcast is ever stamped as ended.
			name: "the marker with no plugin, which is what the tool sends",
			mode: "offline_no_plugin",
		},
		{
			// A title or an address quoting the phrase would otherwise take
			// a broadcasting channel off the air.
			name:    "the marker appears mid-string",
			mode:    "offline_phrase_mid_string",
			wantErr: true,
		},
		{
			name:     "the marker alongside streams",
			mode:     "offline_with_streams",
			wantLive: true,
		},
		{
			name:    "an unrelated failure",
			mode:    "auth_error",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, tt.mode)

			got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Probe() = %+v with err = nil, want the failure surfaced", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe() err = %v, want nil", err)
			}
			if got.Live != tt.wantLive {
				t.Errorf("Live = %t, want %t", got.Live, tt.wantLive)
			}
		})
	}
}

func TestProbe_RefusesATruncatedResponse(t *testing.T) {
	// A prefix of the body is a smaller valid JSON document, and what it
	// parses to is a channel with no streams. Reading one would report a
	// broadcasting channel as offline and skip the recording entirely.
	fakeExec(t, "flooded_streams")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err == nil {
		t.Fatalf("Probe() = %+v with err = nil, want a truncated response refused", got)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("Probe() err = %v, want it to name truncation", err)
	}
}

func TestProbe_NoStreamsIsOffline(t *testing.T) {
	fakeExec(t, "no_streams")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err != nil {
		t.Fatalf("Probe() err = %v, want nil", err)
	}
	if got.Live {
		t.Error("Live = true, want false when no streams are offered")
	}
}

func TestProbe_RealFailureIsNotOffline(t *testing.T) {
	// An expired token reports through the same channel as an offline
	// stream. Treating it as offline would leave a channel silently never
	// recording again, which is the failure mode this whole tool exists to
	// prevent.
	fakeExec(t, "auth_error")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err == nil {
		t.Fatalf("Probe() err = nil (live=%t), want an error for an auth failure", got.Live)
	}
	if !strings.Contains(err.Error(), "token expired") {
		t.Errorf("Probe() err = %q, want it to carry the underlying message", err)
	}
}

func TestProbe_ToleratesAWarningBeforeTheJSON(t *testing.T) {
	fakeExec(t, "live_with_warning")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err != nil {
		t.Fatalf("Probe() err = %v, want nil", err)
	}
	if !got.Live {
		t.Error("Live = false, want true")
	}
}

func TestProbe_UnparseableOutput(t *testing.T) {
	fakeExec(t, "garbage")

	if _, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel"); err == nil {
		t.Error("Probe() err = nil, want an error for unparseable output")
	}
}

func TestProbe_EmptyMetadataIsNotAnError(t *testing.T) {
	// Missing metadata is expected and is precisely why naming happens
	// after capture rather than during it.
	fakeExec(t, "live_no_metadata")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err != nil {
		t.Fatalf("Probe() err = %v, want nil", err)
	}
	if !got.Live {
		t.Error("Live = false, want true")
	}
	if got.Metadata.Title != "" {
		t.Errorf("Title = %q, want empty", got.Metadata.Title)
	}
}

func TestProbe_ResolutionFailure(t *testing.T) {
	engine := &Streamlink{
		resolve: func() (string, error) {
			return "", os.ErrNotExist
		},
	}
	if _, err := engine.Probe(context.Background(), "twitch.tv/examplechannel"); err == nil {
		t.Error("Probe() err = nil, want the resolution failure surfaced")
	}
}

// ///////////////////////////////////////////////
// Capture
// ///////////////////////////////////////////////

func TestCapture(t *testing.T) {
	fakeExec(t, "capture")
	output := filepath.Join(t.TempDir(), "incoming", "twitch-examplechannel-1.ts")

	got, err := fakeEngine().Capture(context.Background(), Request{
		URL: "twitch.tv/examplechannel", Qualities: []string{"1080p60", "best"}, Output: output,
	})
	if err != nil {
		t.Fatalf("Capture() err = %v, want nil", err)
	}
	if got.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", got.ExitCode)
	}
	if got.Bytes != 4096 {
		t.Errorf("Bytes = %d, want 4096", got.Bytes)
	}
	if got.Duration() < 0 {
		t.Errorf("Duration() = %s, want a non-negative span", got.Duration())
	}
	if _, err := os.Stat(output); err != nil {
		t.Errorf("capture output missing: %v", err)
	}
}

func TestCapture_PartialIsReportedNotDiscarded(t *testing.T) {
	// A dropped connection is not a reason to lose what was captured. The
	// non-zero exit belongs in the Result so the caller can decide.
	fakeExec(t, "capture_partial")
	output := filepath.Join(t.TempDir(), "partial.ts")

	got, err := fakeEngine().Capture(context.Background(), Request{
		URL: "twitch.tv/examplechannel", Qualities: []string{"best"}, Output: output,
	})
	if err != nil {
		t.Fatalf("Capture() err = %v, want a Result rather than an error", err)
	}
	if got.ExitCode == 0 {
		t.Error("ExitCode = 0, want the non-zero exit reported")
	}
	if got.Bytes != 512 {
		t.Errorf("Bytes = %d, want the 512 bytes that reached disk", got.Bytes)
	}
}

func TestCapture_ReportsAnUnmeasurableOutput(t *testing.T) {
	// A stat that fails yields Bytes = 0 with nothing said, and the caller
	// writes that zero straight into the row. When the capture also failed
	// the row leaves the pending states, so nothing ever sizes it again: a
	// multi-gigabyte file sits in the incoming directory while its row says
	// it holds nothing, and the space budget trusts that figure.
	fakeExec(t, "capture")
	original := statOutput
	statOutput = func(string) (os.FileInfo, error) {
		return nil, fmt.Errorf("reading %w", os.ErrPermission)
	}
	t.Cleanup(func() { statOutput = original })

	got, err := fakeEngine().Capture(context.Background(), Request{
		URL: "twitch.tv/examplechannel", Qualities: []string{"best"},
		Output: filepath.Join(t.TempDir(), "capture.ts"),
	})
	if err != nil {
		t.Fatalf("Capture() err = %v, want the Result still returned", err)
	}
	if !got.SizeUnknown {
		t.Error("SizeUnknown = false, want an unreadable size told apart from a capture that wrote nothing")
	}
}

func TestCapture_AnOutputTheEngineNeverOpenedIsNotUnknown(t *testing.T) {
	// The companion. A capture that wrote nothing at all really does hold
	// zero bytes, and reporting that as unmeasured would put a warning on
	// every failed capture.
	fakeExec(t, "capture_partial")
	original := statOutput
	statOutput = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { statOutput = original })

	got, err := fakeEngine().Capture(context.Background(), Request{
		URL: "twitch.tv/examplechannel", Qualities: []string{"best"},
		Output: filepath.Join(t.TempDir(), "capture.ts"),
	})
	if err != nil {
		t.Fatalf("Capture() err = %v, want the Result still returned", err)
	}
	if got.SizeUnknown {
		t.Error("SizeUnknown = true for an output that was never opened, want it read as zero bytes")
	}
}

func TestCapture_CancellationStopsTheProcess(t *testing.T) {
	// Cancellation is the path that can leave an engine writing into a file
	// the daemon believes is closed, so what this proves is that the
	// process is gone. It waits for the engine to announce itself before
	// cancelling, because a deadline racing a subprocess spawn measures the
	// machine's load rather than anything about cancellation.
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")
	output := filepath.Join(dir, "hang.ts")
	fakeExec(t, "capture_hang", "FAKE_STARTED="+started, "FAKE_FINISHED="+finished)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type attempt struct {
		result Result
		err    error
	}
	done := make(chan attempt, 1)
	go func() {
		result, err := fakeEngine().Capture(ctx, Request{
			URL: "twitch.tv/examplechannel", Qualities: []string{"best"}, Output: output,
		})
		done <- attempt{result: result, err: err}
	}()

	waitForFile(t, started, "the engine never started")
	cancel()

	var got attempt
	select {
	case got = <-done:
	case <-time.After(procgroup.WaitDelay + 10*time.Second):
		t.Fatal("Capture() never returned after cancellation, so a cancelled capture holds its watcher open")
	}

	if got.err != nil {
		t.Fatalf("Capture() err = %v, want a Result", got.err)
	}
	// The engine writes this only by running its work out to the end, which
	// is what a process that outlived the cancellation does.
	if _, err := os.Stat(finished); err == nil {
		t.Error("the engine ran to completion, want it stopped when the capture was cancelled")
	}
	// The bytes reached disk before the engine announced itself, so this
	// says the caller is given what was captured rather than nothing.
	if got.result.Bytes == 0 {
		t.Error("Bytes = 0, want the bytes written before cancellation counted")
	}
}

// waitForFile blocks until path exists, failing the test if it never does.
func waitForFile(t *testing.T, path, whatWentWrong string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared: %s", path, whatWentWrong)
}

func TestCapture_Rejects(t *testing.T) {
	engine := fakeEngine()
	output := filepath.Join(t.TempDir(), "x.ts")

	tests := []struct {
		name string
		req  Request
	}{
		{name: "no url", req: Request{Qualities: []string{"best"}, Output: output}},
		{name: "no output", req: Request{URL: "twitch.tv/a", Qualities: []string{"best"}}},
		{name: "no qualities", req: Request{URL: "twitch.tv/a", Output: output}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := engine.Capture(context.Background(), tt.req); err == nil {
				t.Error("Capture() err = nil, want a rejection")
			}
		})
	}
}

func TestProbe_ToleratesABraceInsideAWarning(t *testing.T) {
	// Cutting at the first brace anywhere turns a live channel into a parse
	// failure, and the poll loop reads that as a broken channel rather than
	// a broadcast to record.
	fakeExec(t, "live_with_braced_warning")

	got, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err != nil {
		t.Fatalf("Probe() err = %v, want the response found past the warning", err)
	}
	if !got.Live {
		t.Error("Live = false, want true")
	}
}

func TestProbe_BoundsWhatItQuotes(t *testing.T) {
	// The error field is written by the platform and lands in a log line
	// and a notification body.
	fakeExec(t, "flooded_error")

	_, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel")
	if err == nil {
		t.Fatal("Probe() err = nil, want the failure surfaced")
	}
	if len(err.Error()) > 4*procgroup.MaxErrorText {
		t.Errorf("Probe() err is %d bytes, want it bounded near %d", len(err.Error()), procgroup.MaxErrorText)
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("Probe() err = %.80q, want it to keep the start of the message", err)
	}
}

func TestProbe_BoundsTheProbeItself(t *testing.T) {
	// One channel that never answers must not stall the poll loop, which
	// serves every other channel too.
	fakeExec(t, "live")

	var deadline time.Time
	fake := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		deadline, _ = ctx.Deadline()
		return fake(ctx, name, args...)
	}

	if _, err := fakeEngine().Probe(context.Background(), "twitch.tv/examplechannel"); err != nil {
		t.Fatalf("Probe() err = %v, want nil", err)
	}

	if deadline.IsZero() {
		t.Fatal("the probe ran with no deadline, so a hung channel stalls the poll loop")
	}
	if remaining := time.Until(deadline); remaining > probeTimeout {
		t.Errorf("the probe has %s left, want no more than %s", remaining, probeTimeout)
	}
}

func TestCapture_CancellationStopsWhatTheEngineStarted(t *testing.T) {
	// streamlink starts ffmpeg to mux separate video and audio, which is
	// the YouTube and DASH path, so the process writing the recording is
	// not the one that was started here. Killing only the engine leaves
	// that muxer appending to a file this call is about to measure,
	// finalize, remux and rename.
	dir := t.TempDir()
	output := filepath.Join(dir, "capture.ts")
	started := filepath.Join(dir, "started")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeExec(t, "capture_muxer", "FAKE_STARTED="+started)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = fakeEngine().Capture(ctx, Request{
			URL: "twitch.tv/examplechannel", Qualities: []string{"best"}, Output: output,
		})
	}()

	waitForFile(t, started, "the engine never started")
	waitForFile(t, output, "the muxer never wrote to the recording")
	cancel()

	select {
	case <-done:
	case <-time.After(procgroup.WaitDelay + 10*time.Second):
		t.Fatal("Capture() never returned after cancellation")
	}

	before, err := os.Stat(output)
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}

	// Read through the file rather than a pid, because a pid that has been
	// reused answers for a process this test never started.
	time.Sleep(time.Second)

	after, err := os.Stat(output)
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	if after.Size() != before.Size() {
		t.Errorf("the recording grew from %d bytes to %d after the capture was cancelled, want nothing still writing to it",
			before.Size(), after.Size())
	}
}

func TestCapture_RefusesAnExistingOutput(t *testing.T) {
	// Capture reports the size of whatever sits at that path when it
	// finishes. A path holding an earlier recording is reported as bytes
	// this capture wrote, and the earlier recording is counted twice.
	fakeExec(t, "capture")
	output := filepath.Join(t.TempDir(), "already.ts")
	if err := os.WriteFile(output, []byte("an earlier recording"), 0o644); err != nil {
		t.Fatalf("creating %s: %v", output, err)
	}

	if _, err := fakeEngine().Capture(context.Background(), Request{
		URL: "twitch.tv/examplechannel", Qualities: []string{"best"}, Output: output,
	}); err == nil {
		t.Error("Capture() err = nil, want it to refuse a path something already holds")
	}
}

func TestCapture_RefusesOperandsThatReadAsOptions(t *testing.T) {
	// Both values come from config and reach the engine's command line.
	// streamlink parses options wherever they appear, and among them are
	// options that launch a program.
	fakeExec(t, "capture")
	dir := t.TempDir()

	tests := []struct {
		name string
		req  Request
	}{
		{
			name: "a channel that is an option",
			req: Request{
				URL:       "--player=notepad.exe://x",
				Qualities: []string{"best"},
				Output:    filepath.Join(dir, "a.ts"),
			},
		},
		{
			name: "a quality that is an option",
			req: Request{
				URL:       "twitch.tv/examplechannel",
				Qualities: []string{"--player-args=whatever", "best"},
				Output:    filepath.Join(dir, "b.ts"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := fakeEngine().Capture(context.Background(), tt.req); err == nil {
				t.Error("Capture() err = nil, want a rejection")
			}
			if _, err := os.Stat(tt.req.Output); err == nil {
				t.Error("the capture ran, want it refused before the process started")
			}
		})
	}
}

func TestCapture_TerminatesOptionsBeforeTheOperands(t *testing.T) {
	// Driven through Capture rather than through the argument builder,
	// because what matters is the command line the process is actually
	// given.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fakeExec(t, "record_args", "FAKE_ARGS_FILE="+argsFile)

	if _, err := fakeEngine().Capture(context.Background(), Request{
		URL:       "twitch.tv/examplechannel",
		Qualities: []string{"1080p60", "best"},
		Output:    filepath.Join(dir, "out.ts"),
	}); err != nil {
		t.Fatalf("Capture() err = %v, want nil", err)
	}

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded arguments: %v", err)
	}
	args := strings.Split(string(recorded), "\n")

	terminator := slices.Index(args, "--")
	if terminator < 0 {
		t.Fatalf("arguments = %v, want an option terminator before the operands", args)
	}
	for _, operand := range []string{"twitch.tv/examplechannel", "1080p60,best"} {
		if at := slices.Index(args, operand); at < terminator {
			t.Errorf("%q is at %d, before the terminator at %d, so a name reading as an option becomes one",
				operand, at, terminator)
		}
	}
}

func TestCapture_Arguments(t *testing.T) {
	output := filepath.Join(t.TempDir(), "args.ts")
	logPath := filepath.Join(t.TempDir(), "streamlink.log")

	args := fakeEngine().captureArgs(Request{
		URL:       "twitch.tv/examplechannel",
		Qualities: []string{"1080p60", "1080p", "best"},
		Output:    output,
		LogPath:   logPath,
	})
	joined := strings.Join(args, " ")

	// Recording from the earliest buffered segment is what captures a
	// broadcast's opening rather than joining it late.
	for _, want := range []string{"--hls-live-restart", "--output " + output, "--logfile " + logPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("captureArgs() = %v, want it to contain %q", args, want)
		}
	}

	// streamlink takes the ladder as one comma-separated argument, and
	// splitting it would silently record only the first quality.
	if !strings.Contains(joined, "1080p60,1080p,best") {
		t.Errorf("captureArgs() = %v, want the quality ladder joined by commas", args)
	}
}

func TestCapture_OmitsTheLogFlagWithoutAPath(t *testing.T) {
	args := fakeEngine().captureArgs(Request{
		URL: "twitch.tv/a", Qualities: []string{"best"}, Output: "out.ts",
	})
	for _, arg := range args {
		if arg == "--logfile" {
			t.Error("captureArgs() passed --logfile with no path")
		}
	}
}

// ///////////////////////////////////////////////
// The credential config
// ///////////////////////////////////////////////

func TestCaptureArgs_LoadsTheCredentialConfigWithoutNamingTheToken(t *testing.T) {
	// The whole reason the credential travels in a file. A command line is
	// readable by other processes: /proc/<pid>/cmdline on Linux, and WMI by
	// an administrator on Windows. The path may leak. The token may not.
	engine := NewStreamlink(Options{AuthConfig: func() string { return "/data/streamlink-auth.conf" }})

	args := engine.captureArgs(Request{
		URL:    "https://twitch.tv/examplechannel",
		Output: "/library/incoming/capture.ts",
	})

	if !slices.Contains(args, "--config") {
		t.Errorf("captureArgs() = %v, want it to load the credential config", args)
	}
	if !slices.Contains(args, "/data/streamlink-auth.conf") {
		t.Errorf("captureArgs() = %v, want the config path", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, sentinelToken) {
			t.Errorf("the token reached argv: %q", arg)
		}
	}
}

func TestCaptureArgs_NeverDiscardsTheOperatorsOwnConfig(t *testing.T) {
	// --config is additive. Pairing it with --no-config would silently drop
	// whatever the operator configured for themselves.
	engine := NewStreamlink(Options{AuthConfig: func() string { return "/data/streamlink-auth.conf" }})

	args := engine.captureArgs(Request{URL: "https://twitch.tv/examplechannel", Output: "/tmp/x.ts"})

	if slices.Contains(args, "--no-config") {
		t.Errorf("captureArgs() passed --no-config: %v", args)
	}
}

func TestProbe_PicksUpAConfigWrittenAfterConstruction(t *testing.T) {
	// The daemon is built once and runs for months. On Linux and macOS the
	// service starts before anyone has run the auth command, and the daemon
	// itself creates the file-absent state whenever it deletes a rejected
	// token. A path frozen at construction means every capture for the rest
	// of that process records the public ladder, while the auth command
	// reports the credential stored.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fakeExec(t, "record_args", "FAKE_ARGS_FILE="+argsFile)

	stored := ""
	engine := &Streamlink{
		resolve:    func() (string, error) { return "streamlink", nil },
		authConfig: func() string { return stored },
	}

	// The recorded answer is not the point here; the argv is.
	_, _ = engine.Probe(context.Background(), "https://twitch.tv/examplechannel")
	if args := recordedArgs(t, argsFile); slices.Contains(args, "--config") {
		t.Fatalf("arguments = %v, want no --config before a credential exists", args)
	}

	stored = filepath.Join(dir, "streamlink-auth.conf")

	_, _ = engine.Probe(context.Background(), "https://twitch.tv/examplechannel")
	args := recordedArgs(t, argsFile)
	if !slices.Contains(args, "--config") || !slices.Contains(args, stored) {
		t.Errorf("arguments = %v, want the credential config that appeared since construction", args)
	}
}

// recordedArgs reads the argv the fake executable wrote down.
func recordedArgs(t *testing.T, argsFile string) []string {
	t.Helper()

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded arguments: %v", err)
	}
	return strings.Split(string(recorded), "\n")
}

func TestCaptureArgs_PassesNoConfigFlagWithoutACredential(t *testing.T) {
	// The ordinary state before the operator authenticates. A bare --config
	// with no path would consume the next argument as its value.
	engine := NewStreamlink(Options{})

	args := engine.captureArgs(Request{URL: "https://twitch.tv/examplechannel", Output: "/tmp/x.ts"})

	if slices.Contains(args, "--config") {
		t.Errorf("captureArgs() = %v, want no --config without a credential", args)
	}
}

func TestCaptureArgs_NeverRaisesTheLogLevelPastWhatHidesTheToken(t *testing.T) {
	// Measured against streamlink 8.5.0: it echoes every option it loaded,
	// including a --config file's, once the level reaches debug. At error,
	// warning, and info it prints none of them.
	//
	// req.LogPath is the daemon's own rotated log, so a raised level writes
	// the operator's Twitch token into a file that is kept, on every
	// capture. This test is what stops that being a one-line change.
	engine := NewStreamlink(Options{AuthConfig: func() string { return "/data/streamlink-auth.conf" }})

	args := engine.captureArgs(Request{
		URL:     "https://twitch.tv/examplechannel",
		Output:  "/library/incoming/capture.ts",
		LogPath: "/library/.dvr/logs/daemon.log",
	})

	level, found := "", false
	for i, arg := range args {
		if arg == "--loglevel" && i+1 < len(args) {
			level, found = args[i+1], true
		}
	}
	if !found {
		t.Fatalf("captureArgs() = %v, want an explicit loglevel", args)
	}

	// The levels streamlink prints config contents at.
	for _, leaky := range []string{"debug", "trace", "all"} {
		if level == leaky {
			t.Errorf("loglevel = %q, which echoes the credential config into the daemon log", level)
		}
	}
}

// ///////////////////////////////////////////////
// The refused credential
// ///////////////////////////////////////////////

func TestUnauthorized_TellsARefusedCredentialFromEveryOtherFailure(t *testing.T) {
	// Measured against a live channel with a token Twitch refuses: the probe
	// answers `Unauthorized: The "Authorization" token is invalid.` and
	// offers no streams, where no credential at all offers the full ladder.
	// A bad token destroys a capture rather than degrading it, which is why
	// this is worth its own answer.
	tests := []struct {
		name   string
		output probeOutput
		want   bool
	}{
		{
			name:   "the measured refusal",
			output: probeOutput{Plugin: "twitch", Error: `Unauthorized: The "Authorization" token is invalid.`},
			want:   true,
		},
		{
			name:   "an offline channel",
			output: probeOutput{Plugin: "twitch", Error: "No playable streams found on this URL"},
			want:   false,
		},
		{
			name:   "some other failure",
			output: probeOutput{Plugin: "twitch", Error: "Unable to open URL"},
			want:   false,
		},
		{
			// A broadcast title carrying the word must not take a channel
			// off the air, which is why the phrase has to open the error.
			name:   "the word inside a title",
			output: probeOutput{Plugin: "twitch", Error: "Unable to play: Unauthorized streams ep 4"},
			want:   false,
		},
		{
			// The shape a refusal really arrives in. streamlink omits the
			// plugin field from every failure body, so requiring one means
			// ErrUnauthorized is never returned and the credential recheck
			// never runs: a dead cookie costs broadcasts until the hourly
			// timer happens to notice.
			name:   "no plugin field, which is what a refusal carries",
			output: probeOutput{Plugin: "", Error: "Unauthorized"},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unauthorized(tt.output); got != tt.want {
				t.Errorf("unauthorized(%q) = %v, want %v", tt.output.Error, got, tt.want)
			}
		})
	}
}
