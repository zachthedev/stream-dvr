// Package fetch downloads past broadcasts with yt-dlp.
//
// It is the backfill counterpart to internal/record: a driver for one
// external tool and nothing else. It knows nothing of the store, the
// library, or the config, so what a fetched file means is decided by the
// caller and this package only produces one.
//
// The tool reports why a download failed in prose on stderr, not in its
// exit code, so Classify is where that prose becomes a decision. That
// mirrors record.offlineMarker, and for the same reason: the exit code
// alone cannot separate a video that will never appear from one that a
// retry would get.
package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Failure classifies why a fetch did not produce a file.
type Failure int

// Listing is one past broadcast as the tool describes it.
type Listing struct {
	// ID is the platform's own identifier for the stored copy.
	ID string
	// StreamID names the live session the copy came from, when the source
	// reports one. yt-dlp has no field for it, so only a platform API fills
	// this in, and empty means unknown rather than absent.
	StreamID string
	// Title is the broadcast title, as the streamer wrote it. Untrusted.
	Title string
	// URL is where the broadcast can be fetched from.
	URL string
	// StartedAt is when the broadcast began. Zero when the tool reported
	// only a date, which is not a start time and must not be treated as
	// one.
	StartedAt time.Time
	// Duration is how long it ran, or zero when unreported.
	Duration time.Duration
	// IsLive reports that the broadcast is still running. Duration is then a
	// partial reading that grows between calls, so a caller may take
	// StartedAt from such a listing and must never derive an end from it.
	IsLive bool
	// Precise reports whether StartedAt came from a timestamp rather than
	// from a date. A caller records an imprecise listing at a weaker trust
	// level so it cannot displace a time the recorder observed.
	Precise bool
	// Muted is the stretches of the stored copy whose audio the platform
	// replaced with silence, against that copy's own timeline.
	//
	// Nil means nobody could ask: yt-dlp reports no such field, so only a
	// platform API fills this in. An empty list means the platform answered
	// and muted nothing, which is what licenses patching a hole from this
	// copy.
	Muted []MutedSpan
}

// MutedSpan is one stretch of a stored copy the platform silenced.
type MutedSpan struct {
	// Offset is where the stretch begins in the stored copy.
	Offset time.Duration
	// Duration is how long it runs.
	Duration time.Duration
}

// Request describes one download.
type Request struct {
	// URL is the broadcast to fetch.
	URL string
	// Output is the path template, with the extension left to the tool.
	Output string
	// Sections bounds what to fetch, for patching a gap. Empty fetches
	// the whole broadcast.
	Sections string
	// RateLimit caps bandwidth, so a backfill does not saturate the link
	// during a live capture. Empty leaves it uncapped.
	RateLimit string
}

// Result is what a download produced.
type Result struct {
	// Path is the file the tool reported writing.
	Path string
}

// YtDlp fetches past broadcasts.
type YtDlp struct {
	// ffmpeg and ffprobe report where the copies this install validated
	// live.
	//
	// yt-dlp searches its own PATH and finds nothing on a machine where
	// ffmpeg was installed outside it, which is the ordinary case on
	// Windows. Everything that trims or merges a download needs it, so
	// leaving yt-dlp to guess turns gap patching into a failure that
	// repeats every round. Nil leaves yt-dlp to its own search.
	//
	// Both are held because naming a location replaces the search rather
	// than adding to it: yt-dlp takes the directory the ffmpeg sits in and
	// looks there for every other tool it needs, so pointing it at an
	// ffmpeg with no ffprobe beside it loses a tool it would otherwise
	// have found.
	ffmpeg  func() (string, error)
	ffprobe func() (string, error)
	// logger reports what the driver declined to do. Nil says nothing.
	logger *slog.Logger
	// saidNotCoLocated and saidNoFfmpeg record that the driver already
	// reported why it is naming no ffmpeg, so a machine that will answer
	// the same way all day says so once rather than once per download.
	//
	// One each, because they are independent facts about the machine and
	// either can start being true after the other already was. Sharing a
	// latch lets whichever fires first silence the other for good.
	//
	// Pointers, so every copy WithLogger makes shares the one answer. Nil
	// says it every time, which is what a driver built without New does.
	// The lookup itself still runs per download, so an ffmpeg installed
	// while the daemon is up is picked up on the next one.
	saidNotCoLocated *atomic.Bool
	saidNoFfmpeg     *atomic.Bool
}

// ToolError carries what a failed fetch means alongside what it said.
type ToolError struct {
	// Failure is what a caller decides on.
	Failure Failure
	// Excerpt is what the tool wrote, bounded. Untrusted text.
	Excerpt string
	// Err is the process failure underneath.
	Err error
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Failure values.
const (
	// FailureNone means the fetch succeeded.
	FailureNone Failure = iota
	// FailureTransient is worth retrying: a server error, a reset
	// connection, a fragment that did not arrive.
	FailureTransient
	// FailurePermanent will answer the same way forever: the video is
	// removed, private, or restricted where no route exists. Retrying
	// spends a request per pass for good.
	FailurePermanent
	// FailureAuth needs the operator to configure a credential. It is
	// never retried on a timer, because a timer cannot supply one.
	FailureAuth
)

// listTimeout bounds a channel listing. It reads one index page and is
// generous only so a slow link does not fail a whole backfill pass.
const listTimeout = 2 * time.Minute

// infoTimeout bounds one metadata lookup.
const infoTimeout = time.Minute

// maxToolOutput bounds what a listing may spend. A channel with thousands
// of past broadcasts still describes each in a few hundred bytes.
const maxToolOutput = 8 << 20

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

// ErrNoListings reports a channel with no past broadcasts to fetch. It is
// an ordinary answer for a channel that has never streamed, not a fault.
var ErrNoListings = errors.New("the channel has no past broadcasts")

// execCommand builds a subprocess. Tests substitute a helper process so
// the driver is exercised without yt-dlp or a network.
var execCommand = exec.CommandContext

// resolveTool locates yt-dlp, once per call rather than once per process,
// so a tool installed while the daemon runs is found without a restart.
//
// It is a variable for the same reason execCommand is one, and stubbing
// only that one is not enough. A test drives this driver through a helper
// process and never runs the real tool, so resolving a real path first
// would make every one of those tests require yt-dlp on the machine.
var resolveTool = func() (string, error) { return deps.Path(deps.YtDlp) }

// permanentMarkers are the phrases yt-dlp uses for a video that will never
// download.
//
// Matched as text because the exit code does not distinguish them: a
// removed video and a reset connection both exit non-zero. This is the
// same trade record.offlineMarker makes, and it carries the same risk of
// drifting when the tool rewords a message. A phrase that stops matching
// costs a retry loop against something that will never succeed, which is
// why an unrecognized message is retried on a wait that doubles rather than
// forever at one cadence.
var permanentMarkers = []string{
	"video unavailable",
	"this video has been removed",
	"is private",
	"members-only",
	"subscribers only",
	"available in your country",
	"video does not exist",
	"account associated with this video has been terminated",
}

// authMarkers are the phrases meaning a credential would have worked.
var authMarkers = []string{
	"sign in to confirm",
	"requires authentication",
	"login required",
	"use --cookies",
	"private video. sign in",
}

// ///////////////////////////////////////////////
// Classification
// ///////////////////////////////////////////////

// Classify decides what a failed fetch means.
//
// Auth is checked before permanent, because the phrases overlap: a
// members-only video reported as needing a sign-in is one a credential
// fixes, and calling it permanent would bury a fixable problem.
//
// The default is FailureTransient. An unrecognised failure is retried,
// which costs a bounded number of attempts, where treating it as
// permanent would silently abandon a broadcast the operator wanted.
func Classify(stderr string) Failure {
	lowered := strings.ToLower(stderr)

	for _, marker := range authMarkers {
		if strings.Contains(lowered, marker) {
			return FailureAuth
		}
	}
	for _, marker := range permanentMarkers {
		if strings.Contains(lowered, marker) {
			return FailurePermanent
		}
	}
	return FailureTransient
}

// String names the failure, for a log line and for an operator.
func (f Failure) String() string {
	switch f {
	case FailureNone:
		return "none"
	case FailureTransient:
		return "transient"
	case FailurePermanent:
		return "permanent"
	case FailureAuth:
		return "needs a credential"
	default:
		return "unknown"
	}
}

// Terminal reports whether a failure will answer the same way on every
// later attempt.
//
// It belongs with the values because the values are what decide it: a timer
// cannot make a private video public and cannot supply a login, so retrying
// either spends a request per pass for good.
func (f Failure) Terminal() bool {
	return f == FailurePermanent || f == FailureAuth
}

// ///////////////////////////////////////////////
// Driver
// ///////////////////////////////////////////////

// WithLogger returns a copy that reports what it declined to do.
//
// A driver without one says nothing, which is what a caller with no logger
// to give it gets. It mirrors how the store takes one.
func (y YtDlp) WithLogger(logger *slog.Logger) YtDlp {
	y.logger = logger
	return y
}

// sayOnce reports whether a message has yet to be written, claiming the
// right to write it.
//
// The level is asked first. A latch claimed for a record the handler then
// drops spends the one chance the message had, so a driver logging at info
// would swallow its own debug line and stay silent about it forever.
//
// A driver built without New has no latch and says it every time, which is
// what a test wants and what a one-shot command cannot notice.
func (y YtDlp) sayOnce(ctx context.Context, latch *atomic.Bool, level slog.Level) bool {
	if !y.log().Enabled(ctx, level) {
		return false
	}
	return latch == nil || latch.CompareAndSwap(false, true)
}

// log returns the logger to report through, discarding where none was given.
func (y YtDlp) log() *slog.Logger {
	if y.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return y.logger
}

// New returns a fetcher that locates its own tools.
func New() YtDlp {
	return YtDlp{
		ffmpeg:           func() (string, error) { return deps.Path(deps.FFmpeg) },
		ffprobe:          func() (string, error) { return deps.Path(deps.FFprobe) },
		saidNotCoLocated: new(atomic.Bool),
		saidNoFfmpeg:     new(atomic.Bool),
	}
}

// ffmpegLocation names the ffmpeg to hand yt-dlp, or explains why there is
// none to name.
//
// The error is a reason to log, never a refusal. yt-dlp looks for ffmpeg
// itself, so a lookup this package cannot complete leaves the tool as able
// as it is with no flag at all. Refusing on one would be strictly worse
// than saying nothing: the patcher charges an attempt against every gap it
// cannot fill and nothing resets that count, so a handful of rounds would
// retire every hole in the library over a tool yt-dlp can find on its own.
// This package resolves executables more strictly than yt-dlp does, so the
// two disagreeing is ordinary rather than exotic.
//
// Naming a location replaces the search rather than adding to it. Given
// one, yt-dlp looks there for every tool it needs and nowhere else, so a
// path that is wrong or has gone stale leaves it with none where saying
// nothing would have left it a working search. Everything named here is
// therefore confirmed at the moment it is named, and only a co-located
// pair is named at all.
func (y YtDlp) ffmpegLocation(ctx context.Context) (string, error) {
	if y.ffmpeg == nil || y.ffprobe == nil {
		// Nothing was wired up to look, which is not a failure to report.
		return "", nil
	}

	ffmpeg, err := y.ffmpeg()
	if err != nil {
		return "", fmt.Errorf("locating ffmpeg: %w", err)
	}
	ffprobe, err := y.ffprobe()
	if err != nil {
		return "", fmt.Errorf("locating ffprobe: %w", err)
	}
	if ffmpeg == "" || ffprobe == "" {
		return "", errors.New("ffmpeg and ffprobe did not both resolve to a path")
	}
	together, err := sameDir(ffmpeg, ffprobe)
	if err != nil {
		return "", fmt.Errorf("telling whether ffmpeg and ffprobe are in one directory: %w", err)
	}
	if !together {
		// Not a failure, and emphatically not a refusal. Both programs
		// exist and yt-dlp's own search finds them exactly as it did before
		// anything was named. Naming this one would narrow that search to a
		// directory holding no ffprobe, so the flag is dropped and the
		// search is left alone.
		if y.sayOnce(ctx, y.saidNotCoLocated, slog.LevelDebug) {
			y.log().DebugContext(ctx, "ffmpeg and ffprobe are not in one directory, "+
				"so the download tool is left to find them itself",
				slog.String("ffmpeg", filepath.Dir(ffmpeg)),
				slog.String("ffprobe", filepath.Dir(ffprobe)))
		}
		y.foundFfmpeg()
		return "", nil
	}

	// Guarded like every other value this package puts on a command line.
	// deps refuses a non-absolute path, so nothing option-shaped can reach
	// here today, but that invariant lives in another package and this is
	// where the argument is built.
	if err := procgroup.RefuseOption("ffmpeg location", ffmpeg); err != nil {
		return "", err
	}

	// Checked again, at the last instant before the path is committed to an
	// argument, and not redundantly: the flag is exclusive rather than
	// additive. Given one, yt-dlp stops searching, so a path that has gone
	// stale since it resolved leaves the tool with no ffmpeg at all, where
	// saying nothing would have left it the search that finds one. The two
	// stats bracket the window a package upgrade replacing the directory
	// underneath us needs to hit.
	if _, err := os.Stat(ffmpeg); err != nil {
		return "", fmt.Errorf("confirming ffmpeg is still at %s: %w", ffmpeg, err)
	}

	y.foundFfmpeg()
	return ffmpeg, nil
}

// foundFfmpeg re-arms the missing-ffmpeg report, having found one.
//
// The condition can clear and recur across a daemon that runs for months,
// and a latch that never re-arms leaves the second disappearance
// unrecorded. Claimed on every path that resolved an ffmpeg, including the
// one that then declines to name it, because what the latch stands for is
// whether the program was found rather than whether it was named.
func (y YtDlp) foundFfmpeg() {
	if y.saidNoFfmpeg != nil {
		y.saidNoFfmpeg.Store(false)
	}
}

// sameDir reports whether two executables sit in one directory, and errors
// where it could not be established either way.
//
// Compared as directories rather than as text. Windows paths differ in case
// without differing at all, and the two tools are resolved independently:
// one may come from an operator-typed override and the other from a PATH
// entry, so the same directory reaches this in two spellings. A text
// comparison would then refuse a pair that is perfectly co-located, which
// silently reinstates the bug this exists to fix.
//
// A stat that fails is its own answer rather than a false. Reporting it as
// "different directories" states a fact nothing checked, and the caller
// logs that fact: a directory whose permissions changed under a
// long-running daemon then reads as a co-location problem that does not
// exist.
//
// One residue on Windows: os.SameFile reopens each directory to read its
// file id, and reports false where that open is refused. A pair the stats
// reached but the compare could not is still reported as two directories.
// Both answers withhold the flag and leave yt-dlp its own search, so the
// cost is a misleading debug line rather than a lost download.
func sameDir(left, right string) (bool, error) {
	leftDir, rightDir := filepath.Dir(left), filepath.Dir(right)
	if leftDir == rightDir {
		return true, nil
	}

	leftInfo, err := os.Stat(leftDir)
	if err != nil {
		return false, fmt.Errorf("reading the directory holding %s: %w", left, err)
	}
	rightInfo, err := os.Stat(rightDir)
	if err != nil {
		return false, fmt.Errorf("reading the directory holding %s: %w", right, err)
	}
	return os.SameFile(leftInfo, rightInfo), nil
}

// List returns a channel's past broadcasts, newest first.
//
// The listing is flat: it names each broadcast without describing it, so
// one request covers a whole channel. Timestamps come from Info, because a
// flat listing does not carry them.
func (y YtDlp) List(ctx context.Context, channelURL string) ([]Listing, error) {
	if err := procgroup.RefuseOption("channel URL", channelURL); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	output, err := y.run(ctx, "--flat-playlist", "-J", "--no-warnings", "--", channelURL)
	if err != nil {
		return nil, err
	}

	var page struct {
		Entries []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(output), &page); err != nil {
		return nil, fmt.Errorf("reading the listing for %s: %w", channelURL, err)
	}
	if len(page.Entries) == 0 {
		return nil, ErrNoListings
	}

	listings := make([]Listing, 0, len(page.Entries))
	for _, entry := range page.Entries {
		listings = append(listings, Listing{ID: entry.ID, Title: entry.Title, URL: entry.URL})
	}
	return listings, nil
}

// Info returns one broadcast's metadata.
//
// The start time is taken from release_timestamp first, which for a
// livestream is when it began, then from timestamp. A listing carrying
// only upload_date comes back imprecise: a date is not a start time, and
// recording it as one would let an archive displace a time the recorder
// watched happen.
func (y YtDlp) Info(ctx context.Context, url string) (Listing, error) {
	if err := procgroup.RefuseOption("broadcast URL", url); err != nil {
		return Listing{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	output, err := y.run(ctx, "--dump-json", "--skip-download", "--no-warnings", "--", url)
	if err != nil {
		return Listing{}, err
	}

	var video struct {
		ID               string  `json:"id"`
		Title            string  `json:"title"`
		WebpageURL       string  `json:"webpage_url"`
		ReleaseTimestamp int64   `json:"release_timestamp"`
		Timestamp        int64   `json:"timestamp"`
		Duration         float64 `json:"duration"`
		IsLive           bool    `json:"is_live"`
	}
	if err := json.Unmarshal([]byte(output), &video); err != nil {
		return Listing{}, fmt.Errorf("reading the metadata for %s: %w", url, err)
	}

	listing := Listing{
		ID:       video.ID,
		Title:    video.Title,
		URL:      video.WebpageURL,
		Duration: time.Duration(video.Duration) * time.Second,
		IsLive:   video.IsLive,
	}
	if listing.URL == "" {
		listing.URL = url
	}

	switch {
	case video.ReleaseTimestamp > 0:
		listing.StartedAt = time.Unix(video.ReleaseTimestamp, 0).UTC()
		listing.Precise = true
	case video.Timestamp > 0:
		listing.StartedAt = time.Unix(video.Timestamp, 0).UTC()
		listing.Precise = true
	}
	return listing, nil
}

// Playlist returns the media playlist a broadcast is served from.
//
// It exists so a caller can reach a sibling of that address. What the
// address looks like is the platform's business, and this package holds no
// opinion about it: the caller derives what it wants and hands the result
// back to Download, which treats every address alike.
//
// The tool prints one line per stream it would fetch, and the first is the
// one Download would use.
func (y YtDlp) Playlist(ctx context.Context, url string) (string, error) {
	if err := procgroup.RefuseOption("broadcast URL", url); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	output, err := y.run(ctx, "-g", "--no-warnings", "--", url)
	if err != nil {
		return "", err
	}

	address, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	if address == "" {
		return "", fmt.Errorf("no playlist address for %s", url)
	}
	return strings.TrimSpace(address), nil
}

// Download fetches a broadcast and reports the file it wrote.
//
// The output template is literal apart from the extension, so the tool's
// default title-derived name never runs and a remote title cannot reach
// the filesystem. That is structural, not a filter.
//
// A partial file is left where it is. yt-dlp continues one by default, so
// nothing here passes --no-part or --no-continue, and a retry after a
// transient failure resumes rather than starting over.
// requireLiteralOutput refuses an output path holding a yt-dlp field this
// build did not choose.
//
// %(ext)s is the one exception, because the tool fills it from the
// container it produced rather than from anything a broadcast says.
func requireLiteralOutput(output string) error {
	for rest := output; ; {
		open := strings.Index(rest, "%(")
		if open < 0 {
			return nil
		}
		close := strings.Index(rest[open:], ")")
		if close < 0 {
			return fmt.Errorf("the output path holds an unclosed yt-dlp field")
		}
		field := rest[open+2 : open+close]
		if field != "ext" {
			return fmt.Errorf("the output path holds the yt-dlp field %%(%s), which names a file from metadata this did not choose", field)
		}
		rest = rest[open+close:]
	}
}

func (y YtDlp) Download(ctx context.Context, request Request) (Result, error) {
	if request.URL == "" || request.Output == "" {
		return Result{}, errors.New("a download needs a URL and an output path")
	}
	if err := requireWebAddress(request.URL); err != nil {
		return Result{}, err
	}
	if err := procgroup.RefuseOption("broadcast URL", request.URL); err != nil {
		return Result{}, err
	}
	// The doc above calls the template structural rather than a filter,
	// and yt-dlp expands %(field)s inside one. %(ext)s is the structural
	// part: the tool fills it from the container it wrote, which this did
	// choose. Any other field fills it from the broadcast's metadata,
	// which is remote text choosing a filename. The guarantee lived in
	// whichever caller built the stem rather than in the package that
	// states it, so it held only while every caller remembered.
	if err := requireLiteralOutput(request.Output); err != nil {
		return Result{}, err
	}
	if err := procgroup.RefuseOption("output path", request.Output); err != nil {
		return Result{}, err
	}

	// Quiet, because --print writes to stdout and so does the progress
	// meter. Without it the path cannot be told from the last progress
	// line, and a progress line read as a path is a file that does not
	// exist.
	args := []string{"--quiet", "--no-warnings", "--no-overwrites", "-o", request.Output}

	// A hint, never a requirement, and never grounds to refuse: yt-dlp
	// searches for ffmpeg itself, so withholding the flag leaves it exactly
	// as able as it is without one. Worth saying anyway, because this
	// package resolves executables more strictly than yt-dlp does and the
	// operator has no other way to learn the two disagree.
	//
	// Once per driver. A round after a long outage fetches dozens of
	// broadcasts through one of these, and a machine with no ffmpeg would
	// otherwise repeat the same line for every one of them, every six
	// hours, for as long as it stayed that way.
	location, why := y.ffmpegLocation(ctx)
	switch {
	case why != nil:
		if y.sayOnce(ctx, y.saidNoFfmpeg, slog.LevelWarn) {
			// No broadcast named. The condition is a fact about the machine
			// and holds for every download, so quoting whichever one hit it
			// first reads as a problem with that broadcast.
			y.log().WarnContext(ctx, "cannot tell the download tool where ffmpeg is, so it will search for itself",
				slog.Any("reason", why))
		}
	case location != "":
		args = append(args, "--ffmpeg-location", location)
	}
	if request.Sections != "" {
		args = append(args, "--download-sections", request.Sections, "--force-keyframes-at-cuts")
	}
	if request.RateLimit != "" {
		args = append(args, "--limit-rate", request.RateLimit)
	}
	// The terminator is what stops a remote id that leads with a dash from
	// reaching yt-dlp's own option set, which includes options that name a
	// program to run.
	args = append(args, "--print", "after_move:filepath", "--no-simulate", "--", request.URL)

	output, err := y.run(ctx, args...)
	if err != nil {
		return Result{}, err
	}

	// The last non-empty line, because the tool prints progress ahead of
	// it. An empty answer is not an error here: the caller resolves the
	// path from the output template instead, which is why the template is
	// literal.
	path := ""
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			path = trimmed
		}
	}
	return Result{Path: path}, nil
}

// requireWebAddress refuses anything that is not an absolute http or https
// address.
//
// yt-dlp accepts a bare word and treats it as an identifier for whichever
// extractor claims the shape, so a video id handed here downloads somebody
// else's video or fails with a parse error that reads like a network fault.
// The guard is structural: it turns a class of caller mistake into a stated
// refusal before a subprocess runs.
func requireWebAddress(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not an address a download can be pointed at: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%q is not an http or https address", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%q names no host to download from", raw)
	}
	return nil
}

// run executes yt-dlp and returns its standard output.
func (y YtDlp) run(ctx context.Context, args ...string) (string, error) {
	binary, err := resolveTool()
	if err != nil {
		return "", err
	}

	cmd := execCommand(ctx, binary, args...)
	stdout := procgroup.NewOutput(maxToolOutput)
	stderr := procgroup.NewOutput(maxToolOutput)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if runErr := procgroup.Run(cmd); runErr != nil {
		return "", &ToolError{
			Failure: Classify(stderr.String()),
			Excerpt: stderr.Excerpt(procgroup.MaxErrorText),
			Err:     runErr,
		}
	}
	// A truncated answer is not an answer. The bound exists so a tool
	// cannot size this process, and what survives it is a prefix: the JSON
	// readers fail loudly on one, but Download reads the last line as a
	// path and a truncated last line is a path that names a different file.
	if stdout.Truncated() {
		return "", fmt.Errorf("yt-dlp wrote more than %d bytes, so its answer is a fragment", maxToolOutput)
	}
	return stdout.String(), nil
}

// ///////////////////////////////////////////////
// ToolError
// ///////////////////////////////////////////////

// Error implements error.
func (e *ToolError) Error() string {
	return fmt.Sprintf("yt-dlp failed (%s): %v: %s", e.Failure, e.Err, e.Excerpt)
}

// Unwrap exposes the process failure.
func (e *ToolError) Unwrap() error { return e.Err }
