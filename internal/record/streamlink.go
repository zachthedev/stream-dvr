package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Streamlink captures streams by driving the streamlink executable.
type Streamlink struct {
	// resolve locates the executable. It runs per call rather than once,
	// because a package upgrade can move the binary between a probe and
	// the capture that follows it. It takes no context because it starts
	// nothing: locating a file is filesystem work with nothing to cancel.
	resolve func() (string, error)
	// authConfig answers with a streamlink config file holding a credential,
	// or "". This package never learns what is in it and never reads it: the
	// path is all that reaches an argument, because a command line is
	// readable by other processes and a token must not be.
	authConfig func() string
}

// Options configures an engine.
type Options struct {
	// AuthConfig answers with a streamlink config file to load alongside
	// the operator's own, or "" for no credential, which records whatever
	// is public.
	//
	// It is a function rather than a path because an engine is built once
	// and runs for months, while the file comes and goes underneath it: the
	// service starts before anyone has authenticated, the auth command
	// writes the file afterwards, and a rejected credential deletes it. A
	// path resolved at construction freezes whichever of those states the
	// process started in.
	AuthConfig func() string
}

// probeOutput is streamlink's --json response.
type probeOutput struct {
	Plugin   string                     `json:"plugin"`
	Error    string                     `json:"error"`
	Metadata probeMetadata              `json:"metadata"`
	Streams  map[string]json.RawMessage `json:"streams"`
}

// probeMetadata is the metadata block streamlink's plugins expose.
type probeMetadata struct {
	ID       string `json:"id"`
	Author   string `json:"author"`
	Category string `json:"category"`
	Title    string `json:"title"`
}

// captureLogLevel is the most streamlink may say while a credential config
// is loaded.
//
// Anything above this echoes the config's own options back, token included,
// into the log the daemon points it at. Measured against streamlink 8.5.0.
const captureLogLevel = "info"

// offlineMarker is the phrase streamlink opens its error with when a
// channel simply is not broadcasting.
//
// It is matched as text because streamlink reports an offline channel and a
// broken one through the same exit code and the same error field. Treating
// every failure as "offline" would turn an expired token into a channel
// that silently never records again, so anything else is surfaced as an
// error.
const offlineMarker = "No playable streams found"

// unauthorizedMarker opens the error a rejected credential produces.
//
// Measured against a live channel with a token Twitch refuses: the probe
// answers
//
//	Unauthorized: The "Authorization" token is invalid.
//
// and offers no streams at all, where no credential at all offers the full
// ladder. A bad token does not degrade a capture, it destroys it, so this
// is worth telling apart from every other failure.
const unauthorizedMarker = "Unauthorized"

// probeTimeout bounds a liveness check. A probe that hangs must not stall
// the poll loop for every other channel.
const probeTimeout = 60 * time.Second

// maxProbeOutput bounds a probe response. The body is one JSON object
// describing a channel, and everything in it comes from the platform.
const maxProbeOutput = 4 << 20

// ErrUnauthorized reports a probe the platform refused on the credential.
//
// It exists so the daemon can recheck a stored credential at once rather
// than waiting for its hourly tick. Every poll in that window records
// nothing, because a refused token yields no streams.
//
// It is deliberately a little broad: a wrong guess costs one validation
// request, and the check that follows is what actually decides, since only
// a 401 from the provider deletes anything.
var ErrUnauthorized = errors.New("the platform refused the credential")

// execCommand builds the command for an engine invocation. It is a
// variable so tests can substitute a helper process for the real
// executable and exercise exit codes and output without installing
// streamlink.
var execCommand = exec.CommandContext

// statOutput measures a finished capture. It is a variable for the same
// reason execCommand is: a stat that fails for a reason other than the file
// being absent is a permission or filesystem state no portable test can
// arrange, and it is the case whose whole point is not answering zero.
var statOutput = os.Stat

// NewStreamlink returns an engine backed by the streamlink executable.
func NewStreamlink(opts Options) *Streamlink {
	return &Streamlink{
		resolve:    func() (string, error) { return deps.Path(deps.Streamlink) },
		authConfig: opts.AuthConfig,
	}
}

// configArgs loads the credential config, when there is one.
//
// Resolved per invocation, so a credential stored after this engine was
// built is picked up on the next probe rather than at the next restart.
//
// --config is additive rather than replacing, which is what keeps the
// operator's own streamlink configuration working. Never pair it with
// --no-config.
func (s *Streamlink) configArgs() []string {
	if s.authConfig == nil {
		return nil
	}
	path := s.authConfig()
	if path == "" {
		return nil
	}
	return []string{"--config", path}
}

// ///////////////////////////////////////////////
// Probe
// ///////////////////////////////////////////////

// Probe reports whether a URL is broadcasting and what metadata it exposes.
//
// An offline channel is not an error: it returns Probe{Live: false} so the
// caller can keep polling.
func (s *Streamlink) Probe(ctx context.Context, url string) (Probe, error) {
	if err := refuseOption("channel URL", url); err != nil {
		return Probe{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	binary, err := s.resolve()
	if err != nil {
		return Probe{}, err
	}

	// Everything after "--" is an operand, so a channel name that reads as
	// an option cannot become one.
	stdout := procgroup.NewOutput(maxProbeOutput)
	args := append(s.configArgs(), "--json", "--", url)
	cmd := execCommand(ctx, binary, args...)
	cmd.Stdout = stdout
	runErr := procgroup.Run(cmd)

	// A prefix of the body is a smaller valid JSON document, and the
	// reading it yields is a channel with no streams. Parsing it would
	// report a broadcasting channel as offline and skip the recording.
	if stdout.Truncated() {
		return Probe{}, fmt.Errorf("probing %s: the response was truncated at %d bytes", url, maxProbeOutput)
	}

	// streamlink writes its JSON body on both success and failure, so the
	// body is parsed before the exit status is judged.
	var parsed probeOutput
	if jsonErr := json.Unmarshal(trimToJSON(stdout.Bytes()), &parsed); jsonErr != nil {
		if runErr != nil {
			return Probe{}, fmt.Errorf("probing %s: %w", url, runErr)
		}
		return Probe{}, fmt.Errorf("probing %s: parsing response: %w", url, jsonErr)
	}

	// A response that lists streams has streams, whatever its error field
	// and its exit status say alongside them. Every other reading of that
	// contradiction ends in not recording a broadcast that was there.
	if len(parsed.Streams) == 0 {
		return emptyProbe(url, parsed, runErr)
	}

	qualities := make([]string, 0, len(parsed.Streams))
	for name := range parsed.Streams {
		qualities = append(qualities, name)
	}
	sort.Strings(qualities)

	return Probe{
		Live:      true,
		Qualities: qualities,
		Metadata: Metadata{
			ID:       parsed.Metadata.ID,
			Author:   parsed.Metadata.Author,
			Category: parsed.Metadata.Category,
			Title:    parsed.Metadata.Title,
		},
	}, nil
}

// emptyProbe reads a response that listed no streams.
//
// Three readings, and the order matters. An offline channel is not a
// failure. A refused credential is a failure the daemon can act on at
// once, because it costs every poll until the credential is replaced.
// Anything else is a failure the operator has to see.
func emptyProbe(url string, parsed probeOutput, runErr error) (Probe, error) {
	switch {
	case parsed.Error == "" && runErr != nil:
		return Probe{}, fmt.Errorf("probing %s: %w", url, runErr)
	case parsed.Error == "":
		return Probe{Live: false}, nil
	case offline(parsed):
		return Probe{Live: false}, nil
	case unauthorized(parsed):
		return Probe{}, fmt.Errorf("probing %s: %w: %s",
			url, ErrUnauthorized, procgroup.Excerpt(parsed.Error, 0, procgroup.MaxErrorText))
	default:
		return Probe{}, fmt.Errorf("probing %s: %s",
			url, procgroup.Excerpt(parsed.Error, 0, procgroup.MaxErrorText))
	}
}

// offline reports whether an error field describes a channel that is simply
// not broadcasting, as opposed to one this daemon cannot record.
//
// Misreading this in either direction is expensive. Calling a live channel
// offline skips a broadcast silently. Calling an offline channel an error
// backs the poll off and notifies the operator about a channel that is
// merely quiet.
func offline(parsed probeOutput) bool {
	return opensWith(parsed, offlineMarker)
}

// unauthorized reports whether an error field describes a credential the
// platform refused.
func unauthorized(parsed probeOutput) bool {
	return opensWith(parsed, unauthorizedMarker)
}

// opensWith reports whether a probe's error field starts with a marker.
//
// The phrase has to open the error rather than appear anywhere in it, which
// stops a broadcast title or an address carrying those words from deciding
// the outcome.
//
// The marker is the whole test, because streamlink omits the plugin field
// from every failure body. Measured against 8.5.0, an offline channel
// answers with an error and nothing else. The phrase itself is what says a
// plugin resolved: an address nothing understood answers "No plugin can
// handle URL", where this marker is reached only after a plugin answered
// and returned no stream.
func opensWith(parsed probeOutput, marker string) bool {
	return strings.HasPrefix(parsed.Error, marker)
}

// trimToJSON discards anything before the JSON body.
//
// streamlink can emit a warning line ahead of its response, which would
// otherwise make an ordinary probe look like a parse failure. The body
// starts a line of its own, so a brace inside a warning line is not
// mistaken for the start of it.
func trimToJSON(output []byte) []byte {
	for offset := 0; offset < len(output); {
		line := output[offset:]
		end := bytes.IndexByte(line, '\n')
		if end >= 0 {
			line = line[:end]
		}

		if trimmed := bytes.TrimLeft(line, " \t\r"); len(trimmed) > 0 && trimmed[0] == '{' {
			return output[offset+len(line)-len(trimmed):]
		}
		if end < 0 {
			break
		}
		offset += end + 1
	}
	return output
}

// ///////////////////////////////////////////////
// Capture
// ///////////////////////////////////////////////

// Capture records until the broadcast ends or ctx is cancelled.
//
// A non-zero exit is reported in the Result rather than as an error, and
// the error return is reserved for a capture that could not start. Bytes
// already written survive either way: the caller decides what to do with a
// short recording, and it is never this function's place to discard one.
func (s *Streamlink) Capture(ctx context.Context, req Request) (Result, error) {
	if req.URL == "" {
		return Result{}, fmt.Errorf("capture URL is required")
	}
	if req.Output == "" {
		return Result{}, fmt.Errorf("capture output path is required")
	}
	if len(req.Qualities) == 0 {
		return Result{}, fmt.Errorf("capture needs at least one quality")
	}
	if err := refuseOption("capture URL", req.URL); err != nil {
		return Result{}, err
	}
	for _, quality := range req.Qualities {
		if err := refuseOption("capture quality", quality); err != nil {
			return Result{}, err
		}
	}
	if err := paths.RequireAbsent("capture output", req.Output); err != nil {
		return Result{}, err
	}
	// The two values in the argv that sit ahead of the terminator. Both are
	// built by joining onto the library root, so neither begins with a dash
	// today; checked rather than reasoned about, because that reasoning
	// depends on a caller elsewhere staying the way it is, and a value read
	// as an option reaches a tool with options for launching programs.
	if err := refuseOption("capture output", req.Output); err != nil {
		return Result{}, err
	}
	if req.LogPath != "" {
		if err := refuseOption("capture log", req.LogPath); err != nil {
			return Result{}, err
		}
	}

	binary, err := s.resolve()
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(req.Output), 0o755); err != nil {
		return Result{}, fmt.Errorf("creating capture directory: %w", err)
	}

	started := time.Now().UTC()
	cmd := execCommand(ctx, binary, s.captureArgs(req)...)

	// streamlink starts ffmpeg to mux separate video and audio, which every
	// YouTube and DASH source needs. Ending streamlink alone leaves that
	// muxer writing to the output this call is about to measure, finalize,
	// and rename.
	runErr := procgroup.Run(cmd)
	ended := time.Now().UTC()

	// A measurement that could not be made is reported rather than answered
	// as zero. The caller writes this figure straight into the row, and a
	// failed capture never reaches the stage that would correct it, so a
	// confident zero stands for the life of the library while a
	// multi-gigabyte file sits in the incoming directory.
	result := Result{StartedAt: started, EndedAt: ended}
	switch info, statErr := statOutput(req.Output); {
	case statErr == nil:
		result.Bytes = info.Size()
	case errors.Is(statErr, os.ErrNotExist):
		// The engine never opened the output, which is a capture that wrote
		// nothing rather than a size nobody could read.
	default:
		result.SizeUnknown = true
	}

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return result, nil
	case errors.As(runErr, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("running streamlink for %s: %w", req.URL, runErr)
	}
}

// captureArgs builds streamlink's command line.
//
// The log level is part of the credential design, not a preference.
// streamlink echoes every option it loaded, including the ones from a
// --config file, once the level reaches debug. Measured against 8.5.0:
// error, warning, and info print nothing. debug and trace print
//
//	--twitch-api-header=[('Authorization', 'OAuth <token>')]
//
// req.LogPath is a rotated log the library keeps. Raising this past
// captureLogLevel writes the operator's Twitch token into a file that is
// kept, on every capture.
// TestCaptureArgs_NeverRaisesTheLogLevelPastWhatHidesTheToken holds it.
func (s *Streamlink) captureArgs(req Request) []string {
	args := append(s.configArgs(), "--loglevel", captureLogLevel)
	if req.LogPath != "" {
		args = append(args, "--logfile", req.LogPath)
	}

	args = append(args,
		// Start from the earliest buffered segment so the opening minutes
		// of a broadcast are captured rather than joined late.
		"--hls-live-restart",
		// The channel was live when probed. A brief reopen covers the gap
		// between that check and this process starting, without turning
		// capture into an indefinite wait.
		"--retry-open", "3",
		"--output", req.Output,
		// Everything after this is an operand. streamlink parses options
		// wherever they appear, and both values below come from config, so
		// without the terminator a channel name is a way to set any option
		// the tool has, including the ones that launch a program.
		"--",
		req.URL,
		strings.Join(req.Qualities, ","),
	)
	return args
}

// refuseOption rejects a value that a tool would read as an option.
//
// It defers to procgroup, which owns the rule because it owns the spawn.
func refuseOption(what, value string) error {
	return procgroup.RefuseOption(what, value)
}
