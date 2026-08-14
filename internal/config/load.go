package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/generate"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// ValidationError collects every problem found in a config, so one load
// reports all of them rather than making the operator fix them one restart
// at a time.
type ValidationError struct {
	// Problems are the faults found, in field order.
	Problems []Problem
}

// Problem is one invalid field.
type Problem struct {
	// Field is the dotted config path, matching the key in the file.
	Field string
	// Detail says what is wrong and what would be acceptable.
	Detail string
}

// addProblem records one validation failure.
type addProblem func(field, format string, args ...any)

// movedTable names a table this build does not define and the table that
// holds its settings.
type movedTable struct {
	// From is the table name a file may carry.
	From string
	// To is where those settings live.
	To string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// configMode is the mode the config is created with. It can carry a webhook
// URL whose path is the only thing authorizing a post to it, and it lives in
// the operator's own data directory, so nothing else needs to read it.
//
// The mode alone is not the guarantee. It is enforced where the OS has
// modes, and Windows applies nothing from it: Go sets the read-only
// attribute there for a mode with no write bit, and this one has it.
// fsretry.RestrictToOwner is what holds on every platform, and both the
// initial write and every save call it.
const configMode = 0o600

// Bounds on values whose extremes break the daemon rather than merely
// surprising the operator.
const (
	// minPollInterval keeps channel polling from tripping platform rate
	// limits.
	minPollInterval = Duration(5e9)
	// maxPollInterval keeps a short broadcast from being missed entirely.
	maxPollInterval = Duration(600e9)
	// maxConcurrentLimit bounds simultaneous recordings, past which disk
	// throughput, not configuration, is the binding constraint.
	maxConcurrentLimit = 32

	// maxFetchConcurrentLimit bounds simultaneous recovery downloads, well
	// below the recording cap.
	//
	// Each one is admitted against the library total as it stands, with no
	// reservation, so several starting together all measure the same free
	// space and all pass. Nothing stops a download once it is admitted,
	// unlike a capture, which the watermark ends. A low ceiling is what
	// bounds how far past the budget that can carry, and the recorder now
	// starts these rounds with nobody watching.
	maxFetchConcurrentLimit = 4

	// recoveryReach is how far back the recorder's automatic recovery goes,
	// which settle has to sit inside. The recorder owns the behaviour; this
	// is here so a config can be checked without importing the daemon.
	recoveryReach = 14 * 24 * time.Hour
)

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

// ErrNotFound reports a missing config file. Callers distinguish it so a
// first run can write the default config instead of failing.
var ErrNotFound = errors.New("config file not found")

// Containers lists the remux targets the post-processing pipeline supports.
//
// It is exported for the same reason RecompressCodecs is: an interface
// offering the choice has to read the accepted set from here, or it becomes
// a second copy that drifts from what Validate accepts.
var Containers = []string{"mkv", "mp4", "ts"}

// movedTables names the tables whose settings live under [space], so a file
// carrying one is refused with the destination named.
//
// The decoder drops a table it has no field for. A file holding one of these
// would load with every space setting at its default and read as if it set
// them, turning a 512 GB cap into the 2 TB default at the moment the library
// is fullest. Naming the destination is the whole remedy. Nothing here reads
// a value out of one of these tables.
var movedTables = []movedTable{
	{From: "transcode", To: "space.recompress"},
	{From: "retention", To: "space.purge"},
}

// qualityPattern bounds one entry of a streamlink quality ladder. It admits
// the forms streamlink names a stream with, such as 1080p60, best, and
// audio_only, and nothing that could be read as anything else.
var qualityPattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// channelNamePatterns bounds a channel name per platform, because the name
// is spliced into a URL that then becomes an argument to streamlink.
// Anything outside these sets can change which address is fetched.
var channelNamePatterns = map[string]*regexp.Regexp{
	// Twitch logins are up to 25 characters of letters, digits and
	// underscores.
	PlatformTwitch: regexp.MustCompile(`^[A-Za-z0-9_]{1,25}$`),
	// YouTube handles are up to 30 characters and add the period and the
	// hyphen. The "@" belongs to the address, not the name.
	PlatformYouTube: regexp.MustCompile(`^[A-Za-z0-9._-]{1,30}$`),
}

// Error implements error.
func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return fmt.Sprintf("invalid config: %s: %s", e.Problems[0].Field, e.Problems[0].Detail)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "invalid config, %d problems:", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s: %s", p.Field, p.Detail)
	}
	return b.String()
}

// ///////////////////////////////////////////////
// Loading
// ///////////////////////////////////////////////

// Load reads and validates a config file.
//
// Unset fields take their value from DefaultConfig, so a config holding
// only a library root is complete. Returns ErrNotFound when the file does
// not exist, and *ValidationError when it does but is unusable.
func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if err != nil {
		return Config{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close()

	cfg, err := Read(file)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Read decodes and validates a config from any source.
//
// A key the schema does not define is a problem, not a comment. An operator
// who sets a 500 GB cap and mistypes the key otherwise runs on the 2 TB
// default until the disk fills, with a file that reads as if it said so.
//
// That strictness constrains where a format migration can run: it must
// rewrite the file before this, never after. A key a later schema drops is a
// key this refuses, so a migration sitting behind it never runs at all.
func Read(r io.Reader) (Config, error) {
	cfg := DefaultConfig()
	meta, err := toml.NewDecoder(r).Decode(&cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if problem := movedTableProblem(meta); problem != "" {
		return Config{}, errors.New(problem)
	}

	cfg.Normalize()

	problems := unknownKeys(meta)
	var invalid *ValidationError
	if err := cfg.Validate(); errors.As(err, &invalid) {
		problems = append(problems, invalid.Problems...)
	} else if err != nil {
		return Config{}, err
	}
	if len(problems) > 0 {
		return Config{}, &ValidationError{Problems: problems}
	}
	return cfg, nil
}

// movedTableProblem reports a file carrying a table whose settings live
// elsewhere, or "" when it carries none.
//
// It reads the parsed document rather than the undecoded keys, so an empty
// table is caught as surely as a full one, and it runs before validation so
// the operator gets the one instruction that fixes the file instead of a key
// list.
func movedTableProblem(meta toml.MetaData) string {
	present := make(map[string]bool, len(meta.Keys()))
	for _, key := range meta.Keys() {
		if len(key) > 0 {
			present[key[0]] = true
		}
	}

	var moves []string
	for _, moved := range movedTables {
		if present[moved.From] {
			moves = append(moves, fmt.Sprintf("%s moved to %s", moved.From, moved.To))
		}
	}
	if len(moves) == 0 {
		return ""
	}
	return strings.Join(moves, ", ") + "; run 'stream-dvr config init' or rename the tables"
}

// unknownKeys reports every key in the file that the schema does not define.
func unknownKeys(meta toml.MetaData) []Problem {
	undecoded := meta.Undecoded()
	problems := make([]Problem, 0, len(undecoded))
	for _, key := range undecoded {
		problems = append(problems, Problem{
			Field:  key.String(),
			Detail: "is not a config key stream-dvr understands; check the spelling against config.default.toml",
		})
	}
	return problems
}

// Render returns the default config as commented TOML.
//
// It is the single source for both the repository's checked-in template
// and the file "config init" writes, so an operator's starting config can
// never document a different schema than the one in the repository.
func Render() ([]byte, error) {
	return render(DefaultConfig())
}

// render turns any config into the commented TOML this project writes.
//
// Save goes through it too, so a file the settings editor rewrites carries
// the same documentation as the one config init created. An operator whose
// config lost every comment the first time they changed a setting from the
// interface would reasonably stop using the interface.
func render(cfg Config) ([]byte, error) {
	return generate.TOMLConfig{
		ProjectName: "stream-dvr",
		Defaults:    cfg,
		Docs:        Docs,
	}.Generate(generate.OutputEntry{Template: true})
}

// Init writes the commented default config to path.
//
// It refuses to overwrite an existing file, because a config holds an
// operator's channel list and clobbering it silently loses work. The
// exclusive create is the refusal itself, so a file appearing while this
// runs is refused rather than clobbered.
func Init(path string) error {
	content, err := Render()
	if err != nil {
		return fmt.Errorf("rendering default config: %w", err)
	}
	if err := fsretry.MkdirPrivate(filepath.Dir(path), paths.DataDirMode); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	// The exclusive create claims the path and is the refusal, and it is
	// released before anything can fail. Holding it open across the
	// restrict and the write leaves an empty file behind when either one
	// fails or the process is killed between them, and nothing in the tool
	// will write over that: Init refuses because the path exists, and Load
	// refuses because a config with no library root is invalid. The
	// operator is told both at once and no command repairs it.
	claim, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, configMode)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists", path)
	}
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := claim.Close(); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}

	// Published by rename, the way Save does, so the path holds either the
	// whole file or the empty claim this made. A failure now leaves that
	// claim, which the next line removes so a retry can proceed.
	if err := fsretry.WriteFilePrivate(context.Background(), path, content, configMode); err != nil {
		if removed := os.Remove(path); removed != nil && !errors.Is(removed, os.ErrNotExist) {
			return fmt.Errorf("writing %s: %w (and the empty file left behind could not be removed: %w)",
				path, err, removed)
		}
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Save writes a config as TOML, creating parent directories as needed.
//
// One rename publishes the whole file, so no reader ever sees a config that
// parses halfway, and the mode is the one this call asks for rather than
// whatever a file standing at the path already carries.
func Save(path string, cfg Config) error {
	if err := fsretry.MkdirPrivate(filepath.Dir(path), paths.DataDirMode); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	content, err := render(cfg)
	if err != nil {
		return fmt.Errorf("rendering config: %w", err)
	}
	if err := fsretry.WriteFilePrivate(context.Background(), followLink(path), content, configMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// followLink returns the file a config path names, resolving a symbolic link
// to its target.
//
// The write publishes by rename, which replaces a link with a regular file
// and leaves the real config untouched forever after. An operator keeping
// their config in a dotfiles checkout points a link at it, so following one
// is what they asked for.
//
// Only this file does it. fsretry writes credentials too, and there
// replacing a link somebody else planted is the safe answer rather than
// writing through it.
//
// A path naming nothing comes back unchanged, so a first run still creates
// the file.
//
// A link is followed only from inside the operator's own home to a regular
// file inside it. On Linux and macOS write access to a directory is enough
// to plant a link, so following one anywhere would let whoever planted it
// choose the file the next save overwrites, with the operator's channel
// names and library root inside it. Home is the boundary because the case
// this exists for is a dotfiles checkout, which lives there, and an
// attacker who can already write inside it has the account.
//
// Refusing to follow is not refusing to save: the write goes through the
// link's own path instead, which is the answer fsretry gives for
// credentials.
func followLink(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved == path {
		return path
	}

	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return path
	}
	if !underHome(path) || !underHome(resolved) {
		return path
	}
	return resolved
}

// underHome reports whether a path sits inside the operator's home
// directory. A home that cannot be determined admits nothing.
func underHome(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(home, absolute)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// ///////////////////////////////////////////////
// Library root
// ///////////////////////////////////////////////

// LibraryRoot returns the library a config points at.
//
// A config that does not exist yet and one that leaves the root unset both
// report "", because they mean the same thing to a caller about to write
// one. The error is whatever would stop SetLibraryRoot, so a caller can
// refuse before it acts rather than half way through.
func LibraryRoot(path string) (string, error) {
	cfg, err := readRewritable(path)
	if err != nil {
		return "", err
	}
	return cfg.Library.Root, nil
}

// SetLibraryRoot points a config at a library, writing the file when none
// stands at the path yet.
//
// The write happens only while the config still names expect, which is what
// the caller read before it decided to act. Two commands that both found the
// config free would otherwise both create a library, and the second write
// would silently orphan the first one. The window is not closed, only
// narrowed to the moment between this read and the rename that publishes.
//
// An existing file is edited one line at a time: the root's own line is
// replaced and every other byte survives, so the notes and commented-out
// settings an operator wrote are still there afterwards. Rendering the whole
// file keeps every setting and deletes all of that, and the shipped template
// teaches the very style it would delete with its own commented alternatives.
//
// The edit is accepted only once the patched bytes decode to exactly the
// intended config, so a file shaped in a way the line scanner misreads falls
// back to the full render rather than being corrupted.
func SetLibraryRoot(path, root, expect string) error {
	root = strings.TrimSpace(root)
	if problem := LibraryRootProblem(root); problem != "" {
		return &ValidationError{Problems: []Problem{{Field: "library.root", Detail: problem}}}
	}

	current, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if expect != "" {
			return movedUnderneath(path, "", expect)
		}
		want := DefaultConfig()
		want.Library.Root = root
		return Save(path, want)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	want, err := decodeRewritable(bytes.NewReader(current), path)
	if err != nil {
		return err
	}
	if want.Library.Root != expect {
		return movedUnderneath(path, want.Library.Root, expect)
	}
	want.Library.Root = root

	patched, ok := replaceRootLine(current, root)
	if !ok || !decodesTo(patched, want) {
		return Save(path, want)
	}
	if err := fsretry.WriteFilePrivate(context.Background(), followLink(path), patched, configMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// movedUnderneath reports a config that changed between a caller reading it
// and writing it back.
//
// The roots are rendered plainly rather than quoted, because %q doubles every
// separator in a Windows path and the operator has to read this one.
func movedUnderneath(path, found, expect string) error {
	return fmt.Errorf("%s now names %s rather than %s, so run the command again",
		path, describeRoot(found), describeRoot(expect))
}

// describeRoot names a library root for a message, calling an unset one what
// it is rather than rendering an empty pair of quotes.
func describeRoot(root string) string {
	if root == "" {
		return "no library"
	}
	return "library " + root
}

// replaceRootLine rewrites the line assigning root under [library],
// reporting whether it found one.
//
// Only that one shape is handled. A dotted library.root at the top level and
// a value spanning several lines are left to the caller's fallback, because
// the generator writes neither and a scanner that guessed at them would be
// guessing on an operator's only config.
func replaceRootLine(file []byte, root string) ([]byte, bool) {
	lines := strings.SplitAfter(string(file), "\n")
	table := ""

	for i, line := range lines {
		body := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if strings.HasPrefix(body, "[") {
			table = body
			continue
		}
		if table != "[library]" || assignedKey(body) != "root" {
			continue
		}

		value, err := rootAssignment(root)
		if err != nil {
			return nil, false
		}
		// The root's line is the one an operator annotates, because it is
		// where they say which volume this is. Replacing the whole line takes
		// the note with it, and decodesTo cannot object because a comment is
		// not part of what a config decodes to.
		if comment := trailingComment(body); comment != "" {
			value += "  " + comment
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + value + lineEnding(line)
		return []byte(strings.Join(lines, "")), true
	}
	return nil, false
}

// assignedKey returns the bare key a line assigns, or "" for a comment, a
// blank line, or anything else.
func assignedKey(body string) string {
	if body == "" || strings.HasPrefix(body, "#") {
		return ""
	}
	key, _, found := strings.Cut(body, "=")
	if !found {
		return ""
	}
	return strings.TrimSpace(key)
}

// trailingComment returns the comment a line carries after its value, or ""
// when it carries none.
//
// It tracks the quoting rather than cutting at the first "#", because a
// library path may hold one and half a path is not a comment.
func trailingComment(body string) string {
	var quote rune
	escaped := false

	for i, r := range body {
		switch {
		case escaped:
			escaped = false
		case quote == '"' && r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return body[i:]
		}
	}
	return ""
}

// rootAssignment renders the root as a TOML assignment, letting the encoder
// decide the quoting so a backslash or a quote in a path cannot end the
// string early.
func rootAssignment(root string) (string, error) {
	var out strings.Builder
	if err := toml.NewEncoder(&out).Encode(map[string]string{"root": root}); err != nil {
		return "", fmt.Errorf("encoding the library root: %w", err)
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// lineEnding returns the terminator a line carries, which is "" only on a
// final line the file does not end.
func lineEnding(line string) string {
	return line[len(strings.TrimRight(line, "\r\n")):]
}

// decodesTo reports whether a patched file reads back as exactly the config
// intended, which is what makes a line edit safe to publish.
//
// The two are rendered and compared as bytes rather than compared field by
// field. A purge weight is a float64 and may be a NaN, which is not equal to
// itself, so a struct comparison answers "different" for a config nothing
// touched and every write falls back to the full render for the life of that
// file.
func decodesTo(patched []byte, want Config) bool {
	got, err := decodeRewritable(bytes.NewReader(patched), "the patched config")
	if err != nil {
		return false
	}

	gotTOML, gotErr := render(got)
	wantTOML, wantErr := render(want)
	return gotErr == nil && wantErr == nil && bytes.Equal(gotTOML, wantTOML)
}

// readRewritable loads a config that can be written back without losing
// anything the file holds, returning the defaults when no file exists.
func readRewritable(path string) (Config, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close()

	return decodeRewritable(file, path)
}

// decodeRewritable decodes a config and refuses anything a rewrite would
// drop.
//
// A moved table and a key this build does not define both disappear on a full
// render. An operator who mistyped a key and then ran a command that rendered
// the file would lose the setting they meant and the evidence of the typo in
// the same write.
//
// Validation is deliberately not part of this. A root can be set on a config
// whose capture block is out of range, and 'config validate' still reports
// that block afterwards. Refusing here would leave the two problems fixable
// only in an order nothing states.
func decodeRewritable(r io.Reader, name string) (Config, error) {
	cfg := DefaultConfig()
	meta, err := toml.NewDecoder(r).Decode(&cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", name, err)
	}
	if problem := movedTableProblem(meta); problem != "" {
		return Config{}, fmt.Errorf("%s: %s", name, problem)
	}
	if unknown := unknownKeys(meta); len(unknown) > 0 {
		return Config{}, fmt.Errorf("%s: %w", name, &ValidationError{Problems: unknown})
	}

	cfg.Normalize()
	return cfg, nil
}

// ///////////////////////////////////////////////
// Normalization
// ///////////////////////////////////////////////

// Normalize trims the whitespace an editor leaves around a value.
//
// Read calls this before validating, so the value that is checked is the
// value that is used. Trimming only inside Validate leaves a root of
// "  /srv/vods  " passing every check and then being opened untrimmed, and
// leaves "  examplechannel\t" and "examplechannel" as two entries the daemon
// watches separately while the database holds one row for both.
func (c *Config) Normalize() {
	c.Library.Root = strings.TrimSpace(c.Library.Root)
	c.Capture.Container = strings.TrimSpace(c.Capture.Container)
	c.Capture.Quality = trimAll(c.Capture.Quality)
	c.Naming.Template = strings.TrimSpace(c.Naming.Template)
	c.Naming.Timezone = strings.TrimSpace(c.Naming.Timezone)
	c.Space.Recompress.Codec = strings.TrimSpace(c.Space.Recompress.Codec)
	c.Notify.WebhookURL = strings.TrimSpace(c.Notify.WebhookURL)
	c.Twitch.ClientID = strings.TrimSpace(c.Twitch.ClientID)

	for i := range c.Channels {
		c.Channels[i].Platform = strings.TrimSpace(c.Channels[i].Platform)
		c.Channels[i].Name = strings.TrimSpace(c.Channels[i].Name)
		c.Channels[i].Quality = trimAll(c.Channels[i].Quality)
	}
}

// trimAll trims every entry of a list in place and returns it.
func trimAll(values []string) []string {
	for i, value := range values {
		values[i] = strings.TrimSpace(value)
	}
	return values
}

// ///////////////////////////////////////////////
// Validation
// ///////////////////////////////////////////////

// Validate reports every problem with a config at once.
func (c Config) Validate() error {
	var problems []Problem
	add := func(field, format string, args ...any) {
		problems = append(problems, Problem{Field: field, Detail: fmt.Sprintf(format, args...)})
	}

	c.validateVersion(add)
	c.validateLibrary(add)
	c.validateSpace(add)
	c.validateRecompress(add)
	c.validatePurge(add)
	c.validateCapture(add)
	c.validateBackfill(add)
	c.validateNaming(add)
	c.validateNotify(add)
	c.validateChannels(add)
	c.validateTwitch(add)

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func (c Config) validateVersion(add addProblem) {
	switch {
	case c.Version < 1:
		add("schema_version", "must be at least 1")
	case c.Version > SchemaVersion:
		add("schema_version", "is %d but this build understands at most %d; upgrade stream-dvr",
			c.Version, SchemaVersion)
	}
}

func (c Config) validateLibrary(add addProblem) {
	if problem := LibraryRootProblem(c.Library.Root); problem != "" {
		add("library.root", "%s", problem)
	}
}

// LibraryRootProblem reports what is wrong with a library root, or "" when
// it is usable.
//
// The root is the prefix of every path the daemon builds, and those paths
// reach ffmpeg and streamlink argument lists. A relative root resolves
// against whatever directory the daemon starts in, and one written as
// "-vods" makes the first argument of an ffmpeg command an option instead of
// a file. Requiring an absolute path settles both.
//
// It is exported so a command can ask before it acts. A library init that
// created a directory and only then discovered the root was unusable would
// leave a library on disk that no config can name.
func LibraryRootProblem(root string) string {
	root = strings.TrimSpace(root)
	switch problem := textProblem(root); {
	case root == "":
		return "is required; create one with 'stream-dvr library init <path>'"
	case problem != "":
		return problem
	case !filepath.IsAbs(root):
		return "must be an absolute path; a relative one resolves against whatever " +
			"directory the daemon happens to start in"
	}
	return ""
}

func (c Config) validateSpace(add addProblem) {
	if c.Space.MaxSize < 0 {
		add("space.max_size", "must not be negative")
	}
	if c.Space.MinFree < 0 {
		add("space.min_free", "must not be negative")
	}
}

// validateBackfill refuses a backfill block a pass cannot run.
//
// Every field here reaches something that treats it as already sane, and
// the recorder now starts rounds on its own, so a value nothing validated
// can sit in a file for months and then drive unattended downloads. A zero
// attempt cap means retrying one hole inside a captured broadcast forever,
// and a settle longer than the horizon means nothing is ever old enough to
// fetch, which looks from outside exactly like a library with no gaps.
//
// The block is always checked, because there is no switch turning it off.
func (c Config) validateBackfill(add addProblem) {
	if c.Backfill.Settle < 0 {
		add("backfill.settle", "cannot be negative")
	}
	// Past the horizon nothing ever settles, so every round plans nothing
	// and reports success. Recovery would be off with no sign that it was.
	if c.Backfill.Settle.Std() >= recoveryReach {
		add("backfill.settle", "must be less than %s, the furthest back recovery reaches",
			recoveryReach)
	}
	if c.Backfill.MaxConcurrent < 1 {
		add("backfill.max_concurrent", "must be at least 1, or no broadcast is ever fetched")
	}
	if c.Backfill.MaxConcurrent > maxFetchConcurrentLimit {
		add("backfill.max_concurrent", "must be at most %d", maxFetchConcurrentLimit)
	}
	if c.Backfill.MaxAttempts < 1 {
		add("backfill.max_attempts", "must be at least 1, since zero retries a gap that cannot be filled forever")
	}
}

func (c Config) validateCapture(add addProblem) {
	switch {
	case c.Capture.PollInterval < minPollInterval:
		add("capture.poll_interval", "must be at least %s to stay inside platform rate limits", minPollInterval)
	case c.Capture.PollInterval > maxPollInterval:
		add("capture.poll_interval", "must be at most %s or short broadcasts are missed", maxPollInterval)
	}

	if len(c.Capture.Quality) == 0 {
		add("capture.quality", "needs at least one quality, such as \"best\"")
	}
	for i, quality := range c.Capture.Quality {
		if problem := qualityProblem(quality); problem != "" {
			add(fmt.Sprintf("capture.quality[%d]", i), "%s", problem)
		}
	}

	if c.Capture.MinDuration < 0 {
		add("capture.min_duration", "must not be negative")
	}
	if c.Capture.MaxConcurrent < 1 {
		add("capture.max_concurrent", "must be at least 1")
	}
	if c.Capture.MaxConcurrent > maxConcurrentLimit {
		add("capture.max_concurrent", "must be at most %d", maxConcurrentLimit)
	}
	if !slices.Contains(Containers, c.Capture.Container) {
		add("capture.container", "must be one of %s", strings.Join(Containers, ", "))
	}
}

func (c Config) validateNaming(add addProblem) {
	if problem := textProblem(c.Naming.Template); problem != "" {
		add("naming.template", "%s", problem)
	} else if _, err := naming.Parse(c.Naming.Template); err != nil {
		add("naming.template", "%s", err)
	}
	if _, err := c.Location(); err != nil {
		add("naming.timezone", "%s", err)
	}
}

func (c Config) validateRecompress(add addProblem) {
	if !slices.Contains(RecompressCodecs, c.Space.Recompress.Codec) {
		add("space.recompress.codec", "must be one of %s", strings.Join(RecompressCodecs, ", "))
	}
	if c.Space.Recompress.Quality < MinQuality || c.Space.Recompress.Quality > MaxQuality {
		add("space.recompress.quality", "must be between %d and %d", MinQuality, MaxQuality)
	}
	if c.Space.Recompress.After < 0 {
		add("space.recompress.after", "must not be negative")
	}
	if c.Space.Recompress.MaxConcurrent < 1 {
		add("space.recompress.max_concurrent", "must be at least 1")
	}
	if c.Space.Recompress.MaxConcurrent > maxConcurrentLimit {
		add("space.recompress.max_concurrent", "must be at most %d", maxConcurrentLimit)
	}
}

func (c Config) validatePurge(add addProblem) {
	weights := map[string]float64{
		"space.purge.watched_weight":     c.Space.Purge.WatchedWeight,
		"space.purge.age_weight":         c.Space.Purge.AgeWeight,
		"space.purge.refetchable_weight": c.Space.Purge.RefetchableWeight,
	}
	for _, field := range slices.Sorted(maps.Keys(weights)) {
		// NaN and an infinity both answer false to `< 0`, and TOML has a
		// literal for each. Either one makes every candidate score the
		// same, so the ranking falls through to its size tie-break and the
		// purge pane silently lists the largest recording first, which is
		// the opposite of what the weights were set to express.
		if math.IsNaN(weights[field]) || math.IsInf(weights[field], 0) {
			add(field, "must be a finite number")
			continue
		}
		if weights[field] < 0 {
			add(field, "must not be negative")
		}
	}
	if c.Space.Purge.ProtectFor < 0 {
		add("space.purge.protect_for", "must not be negative")
	}
	if c.Space.Purge.TrashGrace < 0 {
		add("space.purge.trash_grace", "must not be negative")
	}
}

// validateTwitch checks the Twitch application this install acts as.
//
// The id reaches two places that treat it differently: a form body, which
// carries whatever it is given, and an HTTP header, which Go trims and then
// refuses outright for a control character. An id that only one of them
// accepts fails on the device flow while a status check still reports one
// configured, so it is refused here where the report is about the file.
func (c Config) validateTwitch(add addProblem) {
	id := c.Twitch.ClientID
	if id == "" {
		return
	}
	if problem := textProblem(id); problem != "" {
		add("twitch.client_id", "%s", problem)
	}
}

func (c Config) validateNotify(add addProblem) {
	address := strings.TrimSpace(c.Notify.WebhookURL)
	if address == "" {
		return
	}
	// url.Parse refuses an ASCII control character and takes an invisible
	// Unicode one, which then reaches the request percent-encoded. The
	// address in the file and the address posted to would be different
	// topics reading identically.
	if problem := textProblem(address); problem != "" {
		add("notify.webhook_url", "%s", problem)
	} else if problem := webAddressProblem(address); problem != "" {
		add("notify.webhook_url", "%s", problem)
	}
}

func (c Config) validateChannels(add addProblem) {
	seen := make(map[string]int, len(c.Channels))

	for i, channel := range c.Channels {
		field := fmt.Sprintf("channels[%d]", i)

		if !slices.Contains(SupportedPlatforms, channel.Platform) {
			add(field+".platform", "must be one of %s", strings.Join(SupportedPlatforms, ", "))
		}
		if problem := channelNameProblem(channel); problem != "" {
			add(field+".name", "%s", problem)
			continue
		}
		// A url channel has no past-broadcast listing to search. Refusing
		// here means an operator who turned backfill on learns at load
		// rather than from a recovery that never happens.
		if channel.Backfill && channel.Platform == PlatformURL {
			add(field+".backfill", "is not supported when platform is %q, "+
				"because there is no listing of past broadcasts to search", PlatformURL)
		}
		// QualityFor substitutes this ladder for the capture-wide one, so it
		// reaches the same argument and needs the same bound.
		for q, quality := range channel.Quality {
			if problem := qualityProblem(quality); problem != "" {
				add(fmt.Sprintf("%s.quality[%d]", field, q), "%s", problem)
			}
		}

		// The key is trimmed and folded because the daemon watches by name
		// and the database stores one row per name. Two entries differing
		// only by whitespace are one channel captured twice, into two files
		// the store cannot collapse.
		key := channel.Platform + "/" + strings.ToLower(strings.TrimSpace(channel.Name))
		if first, duplicate := seen[key]; duplicate {
			add(field, "duplicates channels[%d]; each platform and name pair may appear once", first)
			continue
		}
		seen[key] = i
	}
}

// channelNameProblem reports what is wrong with a channel name, or "" when
// it is usable.
//
// The name reaches streamlink's argv: directly for a url channel, and inside
// a canonical address for the named platforms. A value starting with "-" is
// read there as an option whatever else it says, and --config points
// streamlink at a file that can set --player or --ffmpeg-ffmpeg to any
// executable on the disk. A test for "://" is not a defence: it is satisfied
// by --config=./pwned.conf://x.
//
// A private or link-local address is deliberately allowed. An operator
// pointing at a stream server on their own network is the ordinary case, and
// this file is the operator's own.
func channelNameProblem(channel Channel) string {
	name := strings.TrimSpace(channel.Name)
	switch problem := textProblem(name); {
	case name == "":
		return "is required"
	case strings.HasPrefix(name, "-"):
		return `must not start with "-", which the recorder reads as an option rather than a name`
	case problem != "":
		return problem
	}

	if channel.Platform == PlatformURL {
		if problem := webAddressProblem(name); problem != "" {
			return fmt.Sprintf("%s when platform is %q", problem, PlatformURL)
		}
		return ""
	}

	pattern, known := channelNamePatterns[channel.Platform]
	if !known {
		return ""
	}
	if !pattern.MatchString(name) {
		return fmt.Sprintf("must be a %s channel name matching %s", channel.Platform, pattern)
	}
	return ""
}

// qualityProblem reports what is wrong with one entry of a quality ladder,
// or "" when it is usable.
//
// The ladder reaches streamlink as a single argument with its entries joined
// by commas, so a comma inside an entry is silently two entries, one of them
// a stream name nobody wrote. The pattern admits the forms streamlink names
// a stream with and refuses the leading "-" that reads as an option.
func qualityProblem(quality string) string {
	switch {
	case quality == "":
		return "must not be empty"
	case strings.Contains(quality, ","):
		return "must not hold a comma; the ladder is joined with commas, so one entry would become two"
	case strings.HasPrefix(quality, "-"):
		return `must not start with "-", which the recorder reads as an option rather than a quality`
	case !qualityPattern.MatchString(quality):
		return fmt.Sprintf("must match %s, such as 1080p60, best, or audio_only", qualityPattern)
	}
	return ""
}

// textProblem reports a control or invisible character in a value, or ""
// when it holds none.
//
// A TOML \u0000 escape puts a real NUL in a string, and every check that
// reads what a value says rather than what it holds lets it through.
// Downstream the byte does not survive intact: url.PathEscape writes it as
// "%00", and a C API takes it as the end of the string and works on a
// shorter path than the one configured.
func textProblem(value string) string {
	if strings.IndexFunc(value, notPrintable) < 0 {
		return ""
	}
	return "must not hold control or invisible characters"
}

// webAddressProblem reports what is wrong with a URL, or "" when it is one.
//
// A prefix test is not a parse. It refuses "HTTPS://host", which is the same
// address, and accepts "https:// space" and an address carrying a newline,
// which are not addresses at all.
func webAddressProblem(address string) string {
	parsed, err := url.Parse(address)
	switch {
	case err != nil:
		return "must be a URL that parses"
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return "must start with http:// or https://"
	case parsed.Host == "":
		return "must name a host"
	case parsed.User != nil:
		// A password in the URL reaches every log line and process list
		// that ever renders it. Notification services take their secret in
		// the path or a header instead.
		return "must not carry a username or password; put the secret in the path"
	case linkLocal(parsed.Hostname()):
		// The cloud metadata address answers on this range and hands out
		// credentials to anything that asks. Loopback is deliberately
		// allowed: a self-hosted receiver is the ordinary case for a tool
		// that runs on one machine.
		return "must not be a link-local address"
	}
	return ""
}

// linkLocal reports the addresses that reach a metadata service rather than
// a host the operator chose.
//
// It reads a literal address and nothing else. A name that resolves to one,
// and an integer or a mixed-notation spelling of the same address, are not
// caught here: the only thing that would catch them is a check at dial
// time, against what the address actually resolved to. This is one line of
// defence and the config comment states it as one, because a comment that
// promises more than the code delivers is worse than no comment.
func linkLocal(host string) bool {
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast()
}

// notPrintable reports a rune nothing renders. In a name it means the
// operator cannot tell two entries apart, and neither can a log line.
func notPrintable(r rune) bool { return !unicode.IsPrint(r) }

// ///////////////////////////////////////////////
// Derived values
// ///////////////////////////////////////////////

// Location resolves the configured timezone.
func (c Config) Location() (*time.Location, error) {
	name := strings.TrimSpace(c.Naming.Timezone)
	if name == "" || strings.EqualFold(name, "Local") {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", name)
	}
	return loc, nil
}

// Template parses the naming template. Validate refuses an unparseable one,
// so a caller holding a validated Config can rely on this.
func (c Config) Template() (*naming.Template, error) {
	return naming.Parse(c.Naming.Template)
}

// EnabledChannels returns the channels the daemon watches.
func (c Config) EnabledChannels() []Channel {
	enabled := make([]Channel, 0, len(c.Channels))
	for _, channel := range c.Channels {
		if channel.Enabled {
			enabled = append(enabled, channel)
		}
	}
	return enabled
}

// QualityFor returns the quality ladder for a channel, falling back to the
// capture-wide ladder.
func (c Config) QualityFor(channel Channel) []string {
	if len(channel.Quality) > 0 {
		return channel.Quality
	}
	return c.Capture.Quality
}
