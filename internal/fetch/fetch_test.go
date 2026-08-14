package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Test helpers
// ///////////////////////////////////////////////

// countingLog counts the records a driver wrote, for a test whose subject
// is how often something is said rather than what it said.
//
// Guarded because a slog.Handler must be safe for concurrent use, and a
// handler that is only ever called from one goroutine today is one test
// away from not being.
type countingLog struct {
	mu sync.Mutex
	// level is the floor the handler accepts, so a test can hold a driver
	// to what a production logger would actually keep.
	level   slog.Level
	records int
}

// Enabled implements slog.Handler.
func (c *countingLog) Enabled(_ context.Context, level slog.Level) bool {
	return level >= c.level
}

// WithAttrs implements slog.Handler.
func (c *countingLog) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup implements slog.Handler.
func (c *countingLog) WithGroup(string) slog.Handler { return c }

// Handle implements slog.Handler.
func (c *countingLog) Handle(context.Context, slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records++
	return nil
}

// fakeExec points execCommand at this test binary, which stands in for
// yt-dlp. No network is reached and no tool needs installing.
//
// Resolution is stubbed alongside it, and both halves are required for
// that second claim to hold: the driver resolves the executable before it
// builds the command, so leaving resolution real makes every case here
// pass or fail on whether yt-dlp happens to be on the machine running the
// tests, which is a fact about the machine and not about this code.
func fakeExec(t *testing.T, mode string) {
	t.Helper()

	originalResolve := resolveTool
	resolveTool = func() (string, error) { return "yt-dlp", nil }
	t.Cleanup(func() { resolveTool = originalResolve })

	original := execCommand
	execCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		replay := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], replay...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "FAKE_MODE="+mode)
		return cmd
	}
	t.Cleanup(func() { execCommand = original })
}

// TestHelperProcess stands in for yt-dlp. It is a process entrypoint
// rather than a test, which is the standard library's idiom for driving
// code that spawns a subprocess.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	// The real tool writes its progress meter to stdout, the same place
	// --print writes to, and --quiet is what separates them. Modelling
	// that lets a test notice the flag going missing. Only a download
	// shows a meter. A metadata dump has nothing to report progress on.
	progress := func() {
		if !slices.Contains(os.Args, "--quiet") {
			fmt.Fprintln(os.Stdout, "[download] 100% of 2.00GiB")
		}
	}

	switch os.Getenv("FAKE_MODE") {
	case "list_ok":
		fmt.Fprint(os.Stdout, `{"entries":[
			{"id":"v100001","title":"a broadcast","url":"https://example.com/videos/v100001"},
			{"id":"v100002","title":"another","url":"https://example.com/videos/v100002"}]}`)

	case "list_empty":
		fmt.Fprint(os.Stdout, `{"entries":[]}`)

	case "info_precise":
		fmt.Fprint(os.Stdout, `{"id":"v100001","title":"a broadcast",
			"webpage_url":"https://example.com/videos/v100001",
			"release_timestamp":1772020800,"duration":7200}`)

	case "info_timestamp_only":
		fmt.Fprint(os.Stdout, `{"id":"v100001","title":"a broadcast","timestamp":1772020800}`)

	case "info_no_timestamp":
		fmt.Fprint(os.Stdout, `{"id":"v100001","title":"a broadcast","upload_date":"20260301"}`)

	case "garbage_json":
		fmt.Fprint(os.Stdout, `{"entries": [`)

	// --quiet is passed, so the tool prints the path and nothing else.
	// Without it the progress meter shares stdout and the last line is a
	// percentage rather than a file.
	case "download_ok":
		fmt.Fprintln(os.Stdout, `D:\recordings\incoming\twitch-examplechannel-20260301.mp4`)

	// A version whose print flag reports nothing.
	case "download_silent":
		progress()

	// -g prints one address per stream it would fetch. The first is the one
	// a download would use.
	case "playlist_ok":
		fmt.Fprintln(os.Stdout, "https://cdn.example/vod/chunked/index-muted-ABC123.m3u8")
		fmt.Fprintln(os.Stdout, "https://cdn.example/vod/audio_only/index-muted-ABC123.m3u8")

	case "playlist_empty":
		fmt.Fprintln(os.Stdout, "")

	case "unavailable":
		fmt.Fprintln(os.Stderr, "ERROR: [twitch] v100001: Video unavailable")
		os.Exit(1)

	case "private":
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] EXAMPLEVID01: This video is private")
		os.Exit(1)

	case "auth_required":
		fmt.Fprintln(os.Stderr, "ERROR: Sign in to confirm your age. Use --cookies-from-browser")
		os.Exit(1)

	case "throttled":
		fmt.Fprintln(os.Stderr, "ERROR: unable to download video data: HTTP Error 429: Too Many Requests")
		os.Exit(1)

	case "hang":
		time.Sleep(time.Minute)
	}
}

// ///////////////////////////////////////////////
// Classify
// ///////////////////////////////////////////////

func TestClassify(t *testing.T) {
	// The exit code is the same for every one of these, so this text is
	// the only thing separating a broadcast worth retrying from one that
	// will answer the same way forever.
	tests := []struct {
		name   string
		stderr string
		want   Failure
		why    string
	}{
		{
			name:   "video unavailable",
			stderr: "ERROR: [twitch] v100001: Video unavailable",
			want:   FailurePermanent,
			why:    "a removed broadcast will not come back",
		},
		{
			name:   "private video",
			stderr: "ERROR: This video is private",
			want:   FailurePermanent,
			why:    "privacy is the streamer's decision and no retry changes it",
		},
		{
			name:   "geo restricted",
			stderr: "ERROR: The uploader has not made this video available in your country",
			want:   FailurePermanent,
			why:    "the recorder is not going to move",
		},
		{
			name:   "needs a sign-in",
			stderr: "ERROR: Sign in to confirm your age",
			want:   FailureAuth,
			why:    "a credential fixes it and a timer cannot supply one",
		},
		{
			name:   "suggests cookies",
			stderr: "ERROR: use --cookies-from-browser to pass a login",
			want:   FailureAuth,
			why:    "the tool named the fix, and it is the operator's to apply",
		},
		{
			name: "members-only asking for a sign-in",
			// Matches BOTH lists: "members-only" is a permanent marker and
			// "sign in to confirm" is an auth one. Order is what decides.
			stderr: "ERROR: This video is members-only. Sign in to confirm you are a member",
			want:   FailureAuth,
			why:    "auth is checked first, so a fixable case is not buried as permanent",
		},
		{
			name:   "rate limited",
			stderr: "ERROR: HTTP Error 429: Too Many Requests",
			want:   FailureTransient,
			why:    "backing off is exactly the right response",
		},
		{
			name:   "connection reset",
			stderr: "ERROR: unable to download video data: Connection reset by peer",
			want:   FailureTransient,
			why:    "the network, not the video",
		},
		{
			name:   "server error",
			stderr: "ERROR: HTTP Error 503: Service Unavailable",
			want:   FailureTransient,
			why:    "the platform is having a moment",
		},
		{
			name:   "something nobody has seen",
			stderr: "ERROR: a message this build has never met",
			want:   FailureTransient,
			why:    "an unrecognised failure costs a bounded retry, where calling it permanent abandons a broadcast silently",
		},
		{
			name:   "matched whatever the case",
			stderr: "ERROR: VIDEO UNAVAILABLE",
			want:   FailurePermanent,
			why:    "the tool's capitalisation is not a contract",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.stderr); got != tt.want {
				t.Errorf("Classify() = %v, want %v (%s)", got, tt.want, tt.why)
			}
		})
	}
}

// TestClassify_MarkersAreMatchable holds what an entry in either list must
// be to work at all.
//
// Classify lowercases the tool's output once and compares each marker
// against it untouched, so a marker pasted in yt-dlp's own capitalisation
// compiles, lints, and matches nothing. Its failure then falls to
// FailureTransient, which is indistinguishable from a message the build has
// never met, and the broadcast it was added for is retried forever.
//
// These lists are the file's most edit-prone thing by design, so the rule
// lives beside them rather than in anyone's memory.
func TestClassify_MarkersAreMatchable(t *testing.T) {
	lists := map[string][]string{
		"permanentMarkers": permanentMarkers,
		"authMarkers":      authMarkers,
	}

	for name, markers := range lists {
		for _, marker := range markers {
			t.Run(name+"/"+marker, func(t *testing.T) {
				if marker == "" {
					t.Fatalf("%s holds an empty marker, which strings.Contains matches in every message, so every failure would be classified by this list", name)
				}
				if marker != strings.ToLower(marker) {
					t.Errorf("%s holds %q, which carries a capital letter; Classify compares markers against lowercased output, so this can never match and the failure it was added for is silently retried as transient", name, marker)
				}
			})
		}
	}
}

// ///////////////////////////////////////////////
// Resolution
// ///////////////////////////////////////////////

func TestList_ReportsAToolThatIsNotInstalled(t *testing.T) {
	// A machine without yt-dlp is an ordinary state, and backfill is the
	// only thing that needs it, so this has to be reported rather than
	// crash the recorder.
	//
	// It also pins the seam every other case in this file depends on. A
	// resolution that could not be stubbed would silently require yt-dlp on
	// the machine running the tests, so those cases would pass on a
	// developer's machine and fail on a runner. Stubbing only the command is
	// not enough, and this is what says so.
	original := resolveTool
	resolveTool = func() (string, error) { return "", errors.New("executable not found") }
	t.Cleanup(func() { resolveTool = original })

	_, err := New().List(context.Background(), "https://example.com/examplechannel/videos")
	if err == nil {
		t.Fatal("List() err = nil with no tool installed, want the absence reported")
	}
	if !strings.Contains(err.Error(), "executable not found") {
		t.Errorf("List() err = %v, want it to name what could not be resolved", err)
	}
}

// ///////////////////////////////////////////////
// List
// ///////////////////////////////////////////////

func TestList_ReturnsEveryBroadcast(t *testing.T) {
	fakeExec(t, "list_ok")

	listings, err := New().List(context.Background(), "https://example.com/examplechannel/videos")
	if err != nil {
		t.Fatalf("List() err = %v, want nil", err)
	}
	if len(listings) != 2 {
		t.Fatalf("List() returned %d listings, want 2", len(listings))
	}
	if listings[0].ID != "v100001" {
		t.Errorf("first ID = %q, want %q", listings[0].ID, "v100001")
	}
}

func TestList_ReportsAChannelWithNoBroadcasts(t *testing.T) {
	// An ordinary answer for a channel that has never streamed, so a
	// caller tells it apart from a listing that failed.
	fakeExec(t, "list_empty")

	_, err := New().List(context.Background(), "https://example.com/examplechannel/videos")
	if !errors.Is(err, ErrNoListings) {
		t.Errorf("List() err = %v, want ErrNoListings", err)
	}
}

func TestList_ReportsUnreadableOutput(t *testing.T) {
	fakeExec(t, "garbage_json")

	if _, err := New().List(context.Background(), "https://example.com/c/videos"); err == nil {
		t.Error("List() err = nil for unreadable output, want an error")
	}
}

func TestList_CarriesTheFailureClassification(t *testing.T) {
	// The whole point of the driver: a caller decides on the
	// classification, so it has to survive the error path.
	fakeExec(t, "unavailable")

	_, err := New().List(context.Background(), "https://example.com/c/videos")

	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("List() err = %v, want a *ToolError", err)
	}
	if toolErr.Failure != FailurePermanent {
		t.Errorf("Failure = %v, want %v", toolErr.Failure, FailurePermanent)
	}
}

// ///////////////////////////////////////////////
// Info
// ///////////////////////////////////////////////

func TestPlaylist_ReturnsTheAddressADownloadWouldUse(t *testing.T) {
	// A caller reaches a sibling of this address, so it has to be the one
	// the tool would fetch rather than any of the alternatives it lists.
	fakeExec(t, "playlist_ok")

	address, err := New().Playlist(context.Background(), "https://example.com/videos/v100001")
	if err != nil {
		t.Fatalf("Playlist() err = %v, want nil", err)
	}
	if want := "https://cdn.example/vod/chunked/index-muted-ABC123.m3u8"; address != want {
		t.Errorf("Playlist() = %q, want the first address %q", address, want)
	}
}

func TestPlaylist_ReportsNoAddressAtAll(t *testing.T) {
	// An empty answer is not an address, and returning one would send a
	// caller deriving siblings of nothing.
	fakeExec(t, "playlist_empty")

	if _, err := New().Playlist(context.Background(), "https://example.com/videos/v100001"); err == nil {
		t.Error("Playlist() err = nil, want an empty answer refused")
	}
}

func TestPlaylist_RefusesAnAddressThatLooksLikeAnOption(t *testing.T) {
	// The same guard every exec site in this package carries: an operand
	// opening with a dash reaches the tool's own option set.
	if _, err := New().Playlist(context.Background(), "--config-location"); err == nil {
		t.Error("Playlist() err = nil, want an option-shaped operand refused")
	}
}

func TestInfo_PrefersTheReleaseTimestamp(t *testing.T) {
	// For a livestream that is when it began, which is the only field
	// that lines up with what the recorder would have observed.
	fakeExec(t, "info_precise")

	listing, err := New().Info(context.Background(), "https://example.com/videos/v100001")
	if err != nil {
		t.Fatalf("Info() err = %v, want nil", err)
	}
	if !listing.Precise {
		t.Error("Precise = false for a release timestamp, want true")
	}
	if want := time.Unix(1772020800, 0).UTC(); !listing.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", listing.StartedAt, want)
	}
	if listing.Duration != 2*time.Hour {
		t.Errorf("Duration = %v, want %v", listing.Duration, 2*time.Hour)
	}
}

func TestInfo_FallsBackToTheTimestamp(t *testing.T) {
	fakeExec(t, "info_timestamp_only")

	listing, err := New().Info(context.Background(), "https://example.com/videos/v100001")
	if err != nil {
		t.Fatalf("Info() err = %v, want nil", err)
	}
	if !listing.Precise {
		t.Error("Precise = false for a timestamp, want true")
	}
}

func TestInfo_ReportsADateAsImprecise(t *testing.T) {
	// A date is not a start time. Recorded as one it would displace a time
	// the recorder watched happen, which is what the trust levels exist to
	// prevent.
	fakeExec(t, "info_no_timestamp")

	listing, err := New().Info(context.Background(), "https://example.com/videos/v100001")
	if err != nil {
		t.Fatalf("Info() err = %v, want nil", err)
	}
	if listing.Precise {
		t.Error("Precise = true with only a date, want false")
	}
	if !listing.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want the zero time when no timestamp was reported", listing.StartedAt)
	}
}

func TestInfo_KeepsTheRequestedURLWhenNoneIsReported(t *testing.T) {
	fakeExec(t, "info_timestamp_only")

	const url = "https://example.com/videos/v100001"
	listing, err := New().Info(context.Background(), url)
	if err != nil {
		t.Fatalf("Info() err = %v, want nil", err)
	}
	if listing.URL != url {
		t.Errorf("URL = %q, want the requested %q", listing.URL, url)
	}
}

// ///////////////////////////////////////////////
// Download
// ///////////////////////////////////////////////

func TestDownload_ReportsThePathItWrote(t *testing.T) {
	fakeExec(t, "download_ok")

	result, err := New().Download(context.Background(), Request{
		URL:    "https://example.com/videos/v100001",
		Output: `D:\recordings\incoming\twitch-examplechannel-20260301.%(ext)s`,
	})
	if err != nil {
		t.Fatalf("Download() err = %v, want nil", err)
	}
	if !strings.HasSuffix(result.Path, ".mp4") {
		t.Errorf("Path = %q, want the file the tool reported", result.Path)
	}
}

func TestDownload_ToleratesASilentTool(t *testing.T) {
	// The print flag may report nothing. The caller resolves the path from
	// the output template instead, which is why the template is literal,
	// so an empty answer must not read as a failure.
	fakeExec(t, "download_silent")

	result, err := New().Download(context.Background(), Request{
		URL:    "https://example.com/videos/v100001",
		Output: `D:\recordings\incoming\stem.%(ext)s`,
	})
	if err != nil {
		t.Fatalf("Download() err = %v, want nil", err)
	}
	if result.Path != "" {
		t.Errorf("Path = %q, want empty when the tool reported none", result.Path)
	}
}

func TestDownload_RefusesAnEmptyRequest(t *testing.T) {
	if _, err := New().Download(context.Background(), Request{}); err == nil {
		t.Error("Download() err = nil for an empty request, want a rejection")
	}
}

func TestDownload_RefusesANonURL(t *testing.T) {
	// A bare identifier reaches the tool as a search term or as another
	// site's video id, and the answer that comes back describes somebody
	// else's video or a parse failure that reads like a network fault.
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "a bare numeric id", url: "2847353784", wantErr: true},
		{name: "a bare prefixed id", url: "v2847353784", wantErr: true},
		{name: "a path with no host", url: "/videos/2847353784", wantErr: true},
		{name: "a scheme that is not the web", url: "file:///etc/passwd", wantErr: true},
		{name: "an https address", url: "https://www.twitch.tv/videos/2847353784", wantErr: false},
		{name: "an http address", url: "http://example.com/videos/v100001", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, "download_ok")

			_, err := New().Download(context.Background(), Request{
				URL: tt.url, Output: "out.%(ext)s",
			})
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("Download() err = %v, want an error: %t", err, tt.wantErr)
			}
		})
	}
}

func TestDownload_ClassifiesAnAuthFailure(t *testing.T) {
	fakeExec(t, "auth_required")

	_, err := New().Download(context.Background(), Request{
		URL: "https://example.com/videos/v100001", Output: "out.%(ext)s",
	})

	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("Download() err = %v, want a *ToolError", err)
	}
	if toolErr.Failure != FailureAuth {
		t.Errorf("Failure = %v, want %v", toolErr.Failure, FailureAuth)
	}
}

func TestDownload_ClassifiesAThrottleAsRetryable(t *testing.T) {
	fakeExec(t, "throttled")

	_, err := New().Download(context.Background(), Request{
		URL: "https://example.com/videos/v100001", Output: "out.%(ext)s",
	})

	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("Download() err = %v, want a *ToolError", err)
	}
	if toolErr.Failure != FailureTransient {
		t.Errorf("Failure = %v, want %v", toolErr.Failure, FailureTransient)
	}
}

func TestDownload_StopsWhenTheContextIsCancelled(t *testing.T) {
	// A stalled download must not hold the one backfill slot for as long
	// as the daemon runs.
	fakeExec(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		New().Download(ctx, Request{URL: "https://example.com/v", Output: "out.%(ext)s"}) // the cancellation is what is under test
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Download() ignored a cancelled context")
	}
}

// ///////////////////////////////////////////////
// Failure
// ///////////////////////////////////////////////

func TestFailure_String(t *testing.T) {
	// These reach a log line and an operator, so an unnamed value showing
	// as a bare integer is a report nobody can act on.
	for _, failure := range []Failure{FailureNone, FailureTransient, FailurePermanent, FailureAuth} {
		if got := failure.String(); got == "" || got == "unknown" {
			t.Errorf("Failure(%d).String() = %q, want it named", failure, got)
		}
	}
}

func TestFailure_Terminal(t *testing.T) {
	// A wrong answer costs in both directions. A fixable failure called
	// terminal abandons a broadcast the operator wanted, and a hopeless one
	// called retryable spends a request per pass for good.
	tests := []struct {
		name    string
		failure Failure
		want    bool
		why     string
	}{
		{
			name:    "none",
			failure: FailureNone,
			want:    false,
			why:     "a fetch that succeeded has nothing to give up on",
		},
		{
			name:    "transient",
			failure: FailureTransient,
			want:    false,
			why:     "the platform was having a moment and a retry may work",
		},
		{
			name:    "permanent",
			failure: FailurePermanent,
			want:    true,
			why:     "a removed video answers the same way forever",
		},
		{
			name:    "needs a credential",
			failure: FailureAuth,
			want:    true,
			why:     "a timer cannot supply a credential",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.failure.Terminal(); got != tt.want {
				t.Errorf("Terminal() = %t, want %t (%s)", got, tt.want, tt.why)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Option-shaped operands
// ///////////////////////////////////////////////

// TestYtDlp_RefusesAnOptionShapedURLBeforeSpawningAnything asserts every
// entry point that hands a remote value to yt-dlp refuses one the tool would
// read as an option.
//
// The value is a broadcast id or a channel URL, both of which arrive from the
// platform, and yt-dlp's option set includes --exec and --config-locations.
// The refusal has to land before the spawn, so execCommand is left pointing
// at nothing: a test that reaches it fails by panicking rather than by
// quietly running the tool.
func TestYtDlp_RefusesAnOptionShapedURLBeforeSpawningAnything(t *testing.T) {
	hostile := []struct {
		name  string
		value string
	}{
		{"a long option", "--version"},
		{"a value-taking option", "--config-locations=/tmp/planted.conf"},
		{"a short option", "-J"},
		{"a bare dash", "-"},
	}

	for _, tt := range hostile {
		t.Run(tt.name, func(t *testing.T) {
			var y YtDlp

			if _, err := y.List(context.Background(), tt.value); err == nil {
				t.Errorf("List(%q) err = nil, want a refusal", tt.value)
			}
			if _, err := y.Info(context.Background(), tt.value); err == nil {
				t.Errorf("Info(%q) err = nil, want a refusal", tt.value)
			}
			_, err := y.Download(context.Background(), Request{URL: tt.value, Output: "out.%(ext)s"})
			if err == nil {
				t.Errorf("Download(%q) err = nil, want a refusal", tt.value)
			}
		})
	}
}

func TestDownload_RefusesAnOutputTemplateThatNamesAFileFromMetadata(t *testing.T) {
	// yt-dlp expands %(field)s in -o. %(ext)s is structural, filled from
	// the container the tool produced. Any other field is filled from the
	// broadcast's own metadata, which is a streamer choosing where this
	// writes.
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "the structural extension", output: `incoming/stem.%(ext)s`, want: true},
		{name: "a plain path", output: `incoming/stem.mkv`, want: true},
		{name: "the title", output: `incoming/%(title)s.%(ext)s`, want: false},
		{name: "the uploader", output: `incoming/%(uploader)s/stem.%(ext)s`, want: false},
		{name: "an unclosed field", output: `incoming/%(ext.mkv`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireLiteralOutput(tt.output)
			if accepted := err == nil; accepted != tt.want {
				t.Errorf("requireLiteralOutput(%q) accepted = %t, want %t (%v)",
					tt.output, accepted, tt.want, err)
			}
		})
	}
}

// capturingExec is fakeExec that also keeps the arguments the driver built,
// so a test can assert what the tool was actually told.
func capturingExec(t *testing.T, mode string) *[]string {
	t.Helper()

	originalResolve := resolveTool
	resolveTool = func() (string, error) { return "yt-dlp", nil }
	t.Cleanup(func() { resolveTool = originalResolve })

	captured := new([]string)
	original := execCommand
	execCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		*captured = append([]string(nil), args...)
		replay := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], replay...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "FAKE_MODE="+mode)
		return cmd
	}
	t.Cleanup(func() { execCommand = original })
	return captured
}

// toolIn returns a resolver naming a program inside dir.
//
// The directory is real and the path is joined with the running platform's
// separator, so "beside each other" and "somewhere else" mean the same
// thing wherever the suite runs. A literal like `C:\tools\ffmpeg.exe` is a
// single path element off Windows, which makes every such literal share the
// directory "." and quietly turns the split-install cases into no test at
// all on the two platforms CI also covers.
func toolIn(t *testing.T, dir, name string) func() (string, error) {
	t.Helper()

	// A real file, because the driver confirms a location still exists
	// before it names one. A resolver pointing at nothing is a different
	// fixture, and the test that wants it says so.
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("a stand-in for a program"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return func() (string, error) { return path, nil }
}

func TestDownload_TellsTheToolWhichFfmpegToUse(t *testing.T) {
	// yt-dlp searches its own PATH for ffmpeg and finds nothing on a
	// machine where ffmpeg was installed outside it, which is the ordinary
	// case on Windows. Everything that trims or merges a download needs
	// ffmpeg, so leaving the tool to guess turns patching a gap into a
	// failure that repeats every round against a broadcast that is fine.
	captured := capturingExec(t, "download_ok")
	dir := t.TempDir()
	tool := YtDlp{ffmpeg: toolIn(t, dir, "ffmpeg.exe"), ffprobe: toolIn(t, dir, "ffprobe.exe")}

	if _, err := tool.Download(context.Background(), Request{
		URL:    "https://example.com/videos/v100001",
		Output: `D:\recordings\incoming\stem.%(ext)s`,
	}); err != nil {
		t.Fatalf("Download() err = %v, want nil", err)
	}

	if !slices.Contains(*captured, "--ffmpeg-location") {
		t.Fatalf("args = %v, want the tool told where ffmpeg is", *captured)
	}
	at := slices.Index(*captured, "--ffmpeg-location")
	if want := filepath.Join(dir, "ffmpeg.exe"); at+1 >= len(*captured) || (*captured)[at+1] != want {
		t.Errorf("args = %v, want %q after the flag", *captured, want)
	}
	// Everything after the terminator is an operand. Past it the flag and
	// its path become two more inputs to download, and the tool never reads
	// the option at all.
	if end := slices.Index(*captured, "--"); end >= 0 && at > end {
		t.Errorf("args = %v, want the flag before the operand terminator", *captured)
	}
}

func TestDownload_LeavesTheToolToItsOwnSearchWhenNoneResolves(t *testing.T) {
	// A path that cannot be resolved is worse than none: the tool reports a
	// missing ffmpeg far more clearly than a wrong location would, and a
	// format needing none still downloads.
	tools, elsewhere := t.TempDir(), t.TempDir()

	tests := []struct {
		name    string
		ffmpeg  func() (string, error)
		ffprobe func() (string, error)
	}{
		{name: "no resolvers at all"},
		{name: "resolved to nothing", ffmpeg: func() (string, error) { return "", nil }, ffprobe: toolIn(t, tools, "ffprobe.exe")},
		{
			// Naming this ffmpeg would point the tool at a directory with
			// no ffprobe in it, and the tool looks for every other program
			// it needs beside the ffmpeg it is given. That loses an ffprobe
			// its own search would have found.
			name:    "the pair is not co-located",
			ffmpeg:  toolIn(t, tools, "ffmpeg.exe"),
			ffprobe: toolIn(t, elsewhere, "ffprobe.exe"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := capturingExec(t, "download_ok")
			tool := YtDlp{ffmpeg: tt.ffmpeg, ffprobe: tt.ffprobe}

			if _, err := tool.Download(context.Background(), Request{
				URL:    "https://example.com/videos/v100001",
				Output: `D:\recordings\incoming\stem.%(ext)s`,
			}); err != nil {
				t.Fatalf("Download() err = %v, want nil", err)
			}

			if slices.Contains(*captured, "--ffmpeg-location") {
				t.Errorf("args = %v, want no ffmpeg flag", *captured)
			}
		})
	}
}

func TestDownload_SaysItCannotFindFfmpegOnceRatherThanPerDownload(t *testing.T) {
	// A round after a long outage fetches dozens of broadcasts through one
	// driver. On a machine this package cannot find ffmpeg on, a line per
	// download repeats the same fact every six hours for as long as the
	// machine stays that way, and buries everything else in the log.
	capturingExec(t, "download_ok")
	counted := &countingLog{}
	tool := New().WithLogger(slog.New(counted))
	tool.ffmpeg = func() (string, error) { return "", errors.New("ffmpeg is not installed") }
	tool.ffprobe = toolIn(t, t.TempDir(), "ffprobe.exe")

	for range 3 {
		if _, err := tool.Download(context.Background(), Request{
			URL:    "https://example.com/videos/v100001",
			Output: `D:\recordings\incoming\stem.%(ext)s`,
		}); err != nil {
			t.Fatalf("Download() err = %v, want nil", err)
		}
	}

	counted.mu.Lock()
	defer counted.mu.Unlock()
	if counted.records != 1 {
		t.Errorf("logged %d times over three downloads, want 1", counted.records)
	}
}

func TestDownload_SaysEachReasonItNamesNoFfmpegSeparately(t *testing.T) {
	// Two independent facts about the machine, and either can start being
	// true after the other already was: a split install, and an ffmpeg that
	// stops resolving. Sharing one latch between them lets whichever fires
	// first silence the other for the life of the driver, so a daemon that
	// loses ffmpeg months in never says why gap patching stopped working.
	capturingExec(t, "download_ok")
	counted := &countingLog{level: slog.LevelDebug}
	tool := New().WithLogger(slog.New(counted))
	tool.ffmpeg = toolIn(t, t.TempDir(), "ffmpeg.exe")
	tool.ffprobe = toolIn(t, t.TempDir(), "ffprobe.exe")

	download := func() {
		t.Helper()
		if _, err := tool.Download(context.Background(), Request{
			URL:    "https://example.com/videos/v100001",
			Output: `D:\recordings\incoming\stem.%(ext)s`,
		}); err != nil {
			t.Fatalf("Download() err = %v, want nil", err)
		}
	}

	download()
	tool.ffmpeg = func() (string, error) { return "", errors.New("ffmpeg is not installed") }
	download()

	counted.mu.Lock()
	defer counted.mu.Unlock()
	if counted.records != 2 {
		t.Errorf("logged %d times, want the split install and the lost ffmpeg each reported", counted.records)
	}
}

func TestDownload_NamesNoFfmpegItCannotStillSee(t *testing.T) {
	// The flag replaces yt-dlp's search rather than adding to it: given a
	// location, the tool looks there and nowhere else. A path that resolved
	// and then went, which is what upgrading the package underneath a
	// running daemon leaves, would take ffmpeg away entirely where saying
	// nothing leaves the search that would have found it.
	captured := capturingExec(t, "download_ok")
	gone := filepath.Join(t.TempDir(), "removed")
	tool := New()
	tool.ffmpeg = func() (string, error) { return filepath.Join(gone, "ffmpeg.exe"), nil }
	tool.ffprobe = func() (string, error) { return filepath.Join(gone, "ffprobe.exe"), nil }

	if _, err := tool.Download(context.Background(), Request{
		URL:      "https://example.com/videos/v100001",
		Output:   `D:\recordings\incoming\stem.%(ext)s`,
		Sections: "*00:10:00-00:20:00",
	}); err != nil {
		t.Fatalf("Download() err = %v, want the range downloaded anyway", err)
	}
	if slices.Contains(*captured, "--ffmpeg-location") {
		t.Errorf("args = %v, want no location naming a path that is not there", *captured)
	}
}

func TestNew_ResolvesFfmpegForTheTool(t *testing.T) {
	// The wiring itself. A driver built without both resolvers silently
	// loses the flag, and nothing else in the package would notice.
	tool := New()
	if tool.ffmpeg == nil || tool.ffprobe == nil {
		t.Error("New() built a driver that cannot locate both ffmpeg and ffprobe")
	}
}

func TestDownload_StillCutsARangeWhenItCannotLocateFfmpeg(t *testing.T) {
	// yt-dlp looks for ffmpeg itself, so a lookup this package cannot
	// complete has to leave the download exactly as able as it is with no
	// flag at all. Refusing instead would turn an install this package
	// merely disagrees with into permanent data loss: the patcher charges
	// an attempt against every gap it cannot fill, nothing resets that
	// count, and five rounds retire every hole in the library for good.
	//
	// Every way of failing to resolve, not just the loud one, because a
	// refusal reintroduced on any single branch does the same damage.
	tools := t.TempDir()

	tests := []struct {
		name    string
		ffmpeg  func() (string, error)
		ffprobe func() (string, error)
	}{
		{
			name:    "ffmpeg is not installed",
			ffmpeg:  func() (string, error) { return "", errors.New("ffmpeg is not installed") },
			ffprobe: toolIn(t, tools, "ffprobe.exe"),
		},
		{
			name:    "ffprobe is not installed",
			ffmpeg:  toolIn(t, tools, "ffmpeg.exe"),
			ffprobe: func() (string, error) { return "", errors.New("ffprobe is not installed") },
		},
		{
			name:    "neither resolved to a path",
			ffmpeg:  func() (string, error) { return "", nil },
			ffprobe: func() (string, error) { return "", nil },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := capturingExec(t, "download_ok")
			tool := YtDlp{ffmpeg: tt.ffmpeg, ffprobe: tt.ffprobe}

			_, err := tool.Download(context.Background(), Request{
				URL:      "https://example.com/videos/v100001",
				Output:   `D:\recordings\incoming\stem.%(ext)s`,
				Sections: "*00:10:00-00:20:00",
			})
			if err != nil {
				t.Fatalf("Download() err = %v, want the range downloaded anyway", err)
			}
			if slices.Contains(*captured, "--ffmpeg-location") {
				t.Errorf("args = %v, want no location this package could not resolve", *captured)
			}
		})
	}
}

func TestDownload_StillDownloadsAWholeBroadcastWithoutFfmpeg(t *testing.T) {
	// Only a range needs ffmpeg. A whole download that does not must not be
	// refused over a tool it never uses.
	capturingExec(t, "download_ok")
	tool := YtDlp{
		ffmpeg:  func() (string, error) { return "", errors.New("ffmpeg is not installed") },
		ffprobe: toolIn(t, t.TempDir(), "ffprobe.exe"),
	}

	if _, err := tool.Download(context.Background(), Request{
		URL:    "https://example.com/videos/v100001",
		Output: `D:\recordings\incoming\stem.%(ext)s`,
	}); err != nil {
		t.Errorf("Download() err = %v, want a whole download to proceed", err)
	}
}

func TestDownload_StillCutsARangeWhenTheToolsAreNotCoLocated(t *testing.T) {
	// Both programs exist and yt-dlp finds them by itself, exactly as it
	// did before anything was named. Refusing here would turn a working
	// install into a permanent failure: the patcher charges an attempt
	// against every gap it cannot fill, nothing ever resets that count, and
	// a handful of rounds would retire every hole in the library.
	captured := capturingExec(t, "download_ok")
	tool := YtDlp{
		ffmpeg:  toolIn(t, t.TempDir(), "ffmpeg.exe"),
		ffprobe: toolIn(t, t.TempDir(), "ffprobe.exe"),
	}

	if _, err := tool.Download(context.Background(), Request{
		URL:      "https://example.com/videos/v100001",
		Output:   `D:\recordings\incoming\stem.%(ext)s`,
		Sections: "*00:10:00-00:20:00",
	}); err != nil {
		t.Fatalf("Download() err = %v, want a split install to download anyway", err)
	}
	if slices.Contains(*captured, "--ffmpeg-location") {
		t.Errorf("args = %v, want no location naming a directory with no ffprobe", *captured)
	}
}
