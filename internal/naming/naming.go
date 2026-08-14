// Package naming renders a recording's final path from validated metadata.
//
// It exists to make one failure impossible: a file named from metadata that
// was never fetched. Capture writes under a metadata-free name, and this
// package runs later, once the broadcast's details are known. Every
// placeholder a template uses must resolve to a non-empty value, so a
// missing title blocks the rename rather than producing a name with a hole
// in it. A blocked recording keeps its capture name and waits.
//
// Templates use a closed set of {placeholder} names, not a general template
// language. A bad template is rejected when it is parsed, not when a
// recording finishes at three in the morning.
package naming

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Fields carries every value a template can reference.
type Fields struct {
	// Platform is the source, such as "twitch".
	Platform string
	// Channel is the platform's login for the channel, always known
	// because it is how the broadcast was found.
	Channel string
	// Author is the channel's display name, which may be unavailable.
	Author string
	// Title is the broadcast title.
	Title string
	// Category is the game or category the broadcast was filed under.
	Category string
	// StartedAt is the broadcast's start, in the location the operator
	// wants dates rendered in.
	StartedAt time.Time
	// Extension is the container extension, without a dot.
	Extension string
}

// Result is a rendered path plus what it took to produce it.
type Result struct {
	// Path is the rendered path, relative to the library root, using the
	// host separator.
	Path string
	// Fallbacks names each placeholder that resolved through a fallback
	// rather than its own value. A caller can surface these so a silent
	// metadata gap stays visible.
	Fallbacks []string
	// TitleShortened reports that the title was truncated to fit the
	// filesystem's component limit.
	TitleShortened bool
}

// Template is a parsed, validated naming template.
type Template struct {
	raw   string
	parts []part
	// greedy and lazy are the compiled matchers for reading a rendered path
	// back. Both are built once here rather than per call, because Match runs
	// each of them over every file in a library and compiling is the
	// expensive half of doing so.
	greedy *regexp.Regexp
	lazy   *regexp.Regexp
}

// part is one literal or placeholder in a parsed template.
type part struct {
	literal     string
	placeholder string
}

// MissingFieldError reports placeholders that resolved empty. The rename is
// refused rather than emitting a name with a missing segment.
type MissingFieldError struct {
	// Placeholders names each empty placeholder, sorted.
	Placeholders []string
}

// UnknownPlaceholderError reports a template referencing a placeholder that
// does not exist.
type UnknownPlaceholderError struct {
	// Placeholder is the offending name.
	Placeholder string
	// Known lists every valid placeholder, sorted.
	Known []string
}

// ///////////////////////////////////////////////
// Placeholders
// ///////////////////////////////////////////////

// resolver produces a placeholder's value from a set of fields.
type resolver func(Fields) string

// placeholder is one template value: how it renders, and the shape it takes
// once rendered.
//
// Both directions live in one entry so a placeholder added to the table
// reaches rendering and reading back together. A name the template can write
// but nothing can read is a path this package produces and cannot account
// for.
type placeholder struct {
	// render produces the value from a set of fields.
	render resolver
	// pattern matches one rendered value. A free-text pattern excludes the
	// characters sanitizeValue replaces, because a rendered value cannot
	// hold one, and that exclusion is what keeps a value inside its own
	// path component.
	pattern string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// DefaultTemplate groups recordings by author and year. Forward slashes
// separate directories on every platform and are converted on render.
const DefaultTemplate = "{author}/{year}/{author} - {date} {time} - {title}.{ext}"

// maxSegmentUnits bounds one path component, measured in whichever unit the
// filesystem counts.
//
// Windows allows 255 UTF-16 code units per component and ext4 allows 255
// bytes, and the two disagree about the same text: an astral rune is one
// rune, two UTF-16 units and four UTF-8 bytes. Counting runes therefore
// admits a name of 200 emoji that no filesystem will create, and the
// refusal arrives from the open() a layer below rendering, where nothing
// parks the recording with a reason. The margin leaves room for a
// deduplication suffix.
const maxSegmentUnits = 200

// maxShrinkPasses bounds title shrinking. Each pass removes at least one
// rune, so this only guards against a template that cannot fit at all.
const maxShrinkPasses = 16

// illegal lists the characters Windows forbids in a path component.
// Replacing rather than dropping them keeps word boundaries visible.
const illegal = `<>:"/\|?*`

// replacement substitutes for an illegal character.
const replacement = "_"

// freeText matches a rendered value with no shape of its own: a title, a
// channel, an author, a category.
//
// It excludes exactly what sanitizeValue replaces: every character illegal
// lists, and every control character. Excluding the separator is what holds a
// match inside one path component, so a title can never absorb the directory
// boundary around it. Excluding the control characters keeps Match from
// reading back a value Render could not have written, which is the whole
// claim the two directions make about each other.
const freeText = `[^<>:"/\\|?*\p{Cc}]+`

// ///////////////////////////////////////////////
// Deduplication
// ///////////////////////////////////////////////

// maxPlaceholders bounds how many values one template may name.
//
// A name identifying a broadcast needs a handful. The bound is here
// because Match compiles one capture group per placeholder, and the cost
// of running that grows far faster than the count.
const maxPlaceholders = 32

// maxDuplicates bounds the search for a free name.
const maxDuplicates = 999

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

var (
	// ErrEmptyTemplate reports a template with no content.
	ErrEmptyTemplate = errors.New("naming template is empty")

	// ErrUnclosedPlaceholder reports a "{" with no matching "}".
	ErrUnclosedPlaceholder = errors.New("naming template has an unclosed placeholder")

	// ErrNoPlaceholders reports a template that would name every
	// recording identically.
	ErrNoPlaceholders = errors.New("naming template has no placeholders")

	// ErrTooManyPlaceholders reports a template too large to read a name
	// back from in reasonable time.
	ErrTooManyPlaceholders = errors.New("naming template has too many placeholders")

	// ErrEmptySegment reports a path component that sanitized away to
	// nothing.
	ErrEmptySegment = errors.New("naming template produced an empty path segment")

	// ErrCannotFit reports a component that exceeds the filesystem's limit
	// even after the title is removed.
	ErrCannotFit = errors.New("naming template cannot fit the filesystem path limit")
)

// placeholders maps each name to how it renders and how it reads back.
// Adding an entry here is all it takes to make a placeholder available to
// templates.
var placeholders = map[string]placeholder{
	"platform": {func(f Fields) string { return f.Platform }, freeText},
	"channel":  {func(f Fields) string { return f.Channel }, freeText},
	"author":   {func(f Fields) string { return f.Author }, freeText},
	"title":    {func(f Fields) string { return f.Title }, freeText},
	"category": {func(f Fields) string { return f.Category }, freeText},
	"date":     {func(f Fields) string { return f.StartedAt.Format("2006-01-02") }, `\d{4}-\d{2}-\d{2}`},
	"time":     {func(f Fields) string { return f.StartedAt.Format("15-04") }, `\d{2}-\d{2}`},
	"year":     {func(f Fields) string { return f.StartedAt.Format("2006") }, `\d{4}`},
	"month":    {func(f Fields) string { return f.StartedAt.Format("01") }, `\d{2}`},
	"day":      {func(f Fields) string { return f.StartedAt.Format("02") }, `\d{2}`},
	"ext":      {func(f Fields) string { return strings.TrimPrefix(f.Extension, ".") }, `[A-Za-z0-9]+`},
}

// ///////////////////////////////////////////////
// Sanitization
// ///////////////////////////////////////////////

// reservedNames lists Windows device names, which cannot be used as a path
// component even with an extension. They are prefixed on every platform so
// a library stays portable.
var reservedNames = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {},
	"com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {},
	"lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// reservedSegments lists the directories a library keeps for itself, taken
// from paths so there is one definition of what a library owns.
var reservedSegments = func() map[string]struct{} {
	reserved := make(map[string]struct{}, len(paths.ReservedDirNames))
	for _, name := range paths.ReservedDirNames {
		reserved[strings.ToLower(name)] = struct{}{}
	}
	return reserved
}()

// Error implements error.
func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("recording metadata is missing %s", strings.Join(e.Placeholders, ", "))
}

// Error implements error.
func (e *UnknownPlaceholderError) Error() string {
	return fmt.Sprintf("unknown placeholder {%s}, valid placeholders are %s",
		e.Placeholder, strings.Join(e.Known, ", "))
}

// Placeholders returns every valid placeholder name, sorted.
func Placeholders() []string {
	names := make([]string, 0, len(placeholders))
	for name := range placeholders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ///////////////////////////////////////////////
// Parsing
// ///////////////////////////////////////////////

// Parse validates a template and prepares it for rendering.
//
// Every placeholder must be known, so a typo surfaces when configuration is
// loaded rather than when a broadcast ends.
func Parse(raw string) (*Template, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, ErrEmptyTemplate
	}

	var (
		parts []part
		count int
		rest  = raw
	)
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			if rest != "" {
				parts = append(parts, part{literal: rest})
			}
			break
		}
		if open > 0 {
			parts = append(parts, part{literal: rest[:open]})
		}

		closed := strings.IndexByte(rest[open:], '}')
		if closed < 0 {
			return nil, fmt.Errorf("%q: %w", raw, ErrUnclosedPlaceholder)
		}

		name := rest[open+1 : open+closed]
		if _, ok := placeholders[name]; !ok {
			return nil, &UnknownPlaceholderError{Placeholder: name, Known: Placeholders()}
		}
		parts = append(parts, part{placeholder: name})
		count++
		rest = rest[open+closed+1:]
	}

	if count == 0 {
		return nil, fmt.Errorf("%q: %w", raw, ErrNoPlaceholders)
	}
	// Bounded because reading a name back costs roughly the cube of the
	// capture count, and a template is operator config that nothing else
	// checks. Past this a single filename takes seconds to match and the
	// import has no way to interrupt itself mid-file.
	if count > maxPlaceholders {
		return nil, fmt.Errorf("%q uses %d placeholders and no name needs more than %d: %w",
			raw, count, maxPlaceholders, ErrTooManyPlaceholders)
	}

	template := &Template{raw: raw, parts: parts}
	template.greedy = template.compile(false)
	template.lazy = template.compile(true)
	return template, nil
}

// String returns the template's source text.
func (t *Template) String() string { return t.raw }

// Uses reports whether the template references a placeholder.
func (t *Template) Uses(name string) bool {
	for _, p := range t.parts {
		if p.placeholder == name {
			return true
		}
	}
	return false
}

// ///////////////////////////////////////////////
// Rendering
// ///////////////////////////////////////////////

// Render produces the recording's path relative to the library root.
//
// It returns *MissingFieldError when any placeholder the template uses
// resolves empty. That is the guard against a partially-named file: the
// caller leaves the recording under its capture name and retries once
// metadata arrives.
func (t *Template) Render(fields Fields) (Result, error) {
	resolved, fallbacks := resolve(fields)

	if missing := t.missingPlaceholders(resolved); len(missing) > 0 {
		return Result{}, &MissingFieldError{Placeholders: missing}
	}

	// Values are made separator-free before substitution, so only the
	// template's own literals can introduce a path component. Without this
	// a title containing "../" would place the recording outside the
	// library root.
	safe := make(map[string]string, len(resolved))
	for name, value := range resolved {
		safe[name] = sanitizeValue(value)
	}

	rendered, shortened, err := t.renderFitting(safe)
	if err != nil {
		return Result{}, err
	}

	segments, err := sanitizeSegments(rendered)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Path:           filepath.Join(segments...),
		Fallbacks:      fallbacks,
		TitleShortened: shortened,
	}, nil
}

// resolve computes every placeholder's value and applies fallbacks.
//
// Author falls back to the channel login. The login is always known because
// it is how the broadcast was found, so a display name that failed to load
// degrades the name rather than blocking it.
func resolve(fields Fields) (map[string]string, []string) {
	var fallbacks []string
	if strings.TrimSpace(fields.Author) == "" && strings.TrimSpace(fields.Channel) != "" {
		fields.Author = fields.Channel
		fallbacks = append(fallbacks, "author")
	}

	values := make(map[string]string, len(placeholders))
	for name, entry := range placeholders {
		values[name] = strings.TrimSpace(entry.render(fields))
	}
	return values, fallbacks
}

// missingPlaceholders lists the template's placeholders that resolved
// empty, sorted and deduplicated.
func (t *Template) missingPlaceholders(values map[string]string) []string {
	seen := make(map[string]struct{})
	for _, p := range t.parts {
		if p.placeholder == "" {
			continue
		}
		if values[p.placeholder] == "" {
			seen[p.placeholder] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}

	missing := make([]string, 0, len(seen))
	for name := range seen {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}

// renderFitting renders the template, shrinking the title until every path
// component fits the filesystem's limit.
//
// The title is the only elastic value. Everything else is either short by
// construction (dates, extensions) or load-bearing for identifying the
// recording (author, channel).
func (t *Template) renderFitting(values map[string]string) (string, bool, error) {
	working := make(map[string]string, len(values))
	maps.Copy(working, values)

	for range maxShrinkPasses {
		rendered := t.expand(working)
		overflow := longestOverflow(rendered)
		if overflow == 0 {
			return rendered, working["title"] != values["title"], nil
		}
		if working["title"] == "" {
			return "", false, fmt.Errorf("%q: %w", t.raw, ErrCannotFit)
		}
		working["title"] = shrink(working["title"], overflow)
	}
	return "", false, fmt.Errorf("%q: %w", t.raw, ErrCannotFit)
}

// expand substitutes placeholder values into the template.
func (t *Template) expand(values map[string]string) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.placeholder == "" {
			b.WriteString(p.literal)
			continue
		}
		b.WriteString(values[p.placeholder])
	}
	return b.String()
}

// toSlash normalizes both separator styles to a forward slash. A template is
// authored once and read on another operating system, so it must split into
// the same components everywhere rather than follow the host's separator.
func toSlash(s string) string {
	return strings.ReplaceAll(s, `\`, "/")
}

// longestOverflow returns how many runes the longest path component
// exceeds the limit by, or zero when every component fits.
func longestOverflow(rendered string) int {
	worst := 0
	for segment := range strings.SplitSeq(toSlash(rendered), "/") {
		if over := segmentUnits(segment) - maxSegmentUnits; over > worst {
			worst = over
		}
	}
	return worst
}

// segmentUnits measures a component the way the strictest filesystem does.
//
// The worst of the two counts, because one name has to fit both: a library
// written on one platform is read on the other. Runes are counted as well,
// so a shrink that removes runes always makes progress against the figure
// it is shrinking towards.
func segmentUnits(segment string) int {
	units := 0
	for _, r := range segment {
		units = max(units, 1)
		if r > 0xFFFF {
			// Two UTF-16 units, four UTF-8 bytes.
			units += 4
			continue
		}
		units += utf8.RuneLen(r)
	}
	return units
}

// shrink removes at least overflow runes from the end of a title, cutting
// back to a word boundary when one is close enough to be worth keeping.
func shrink(title string, overflow int) string {
	runes := []rune(title)
	keep := len(runes) - overflow
	if keep <= 0 {
		return ""
	}

	trimmed := strings.TrimRight(string(runes[:keep]), " \t-_")

	// Prefer a word boundary, since a name ending mid-word reads as
	// corruption. Give up at most a quarter of what remains to reach one,
	// which keeps a single long word from erasing the whole title.
	if cut := strings.LastIndexByte(trimmed, ' '); cut > 0 {
		if (len(trimmed)-cut)*4 <= len(trimmed) {
			trimmed = trimmed[:cut]
		}
	}
	return strings.TrimRight(trimmed, " \t-_")
}

// sanitizeSegments splits a rendered template on its forward slashes and
// makes each component safe to create on disk.
func sanitizeSegments(rendered string) ([]string, error) {
	raw := strings.Split(toSlash(rendered), "/")
	segments := make([]string, 0, len(raw))

	for _, segment := range raw {
		// A template with a doubled or trailing slash yields empty pieces
		// that carry no meaning, so they are dropped rather than refused.
		if segment == "" {
			continue
		}
		clean, err := SanitizeSegment(segment)
		if err != nil {
			return nil, err
		}
		segments = append(segments, clean)
	}

	if len(segments) == 0 {
		return nil, ErrEmptySegment
	}
	return segments, nil
}

// sanitizeValue replaces every character that is illegal in a path
// component, including both separators. Applied to a placeholder's value
// before substitution, it guarantees the value lands in exactly one path
// component.
func sanitizeValue(value string) string {
	var b strings.Builder
	b.Grow(len(value))

	for _, r := range value {
		switch {
		case strings.ContainsRune(illegal, r), unicode.IsControl(r):
			b.WriteString(replacement)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SanitizeSegment makes one path component safe on every supported
// filesystem. It replaces illegal and control characters. It removes
// trailing dots and spaces, which Windows silently strips. It prefixes a
// device name and a name the library keeps for itself.
func SanitizeSegment(segment string) (string, error) {
	// Trimmed to a fixed point rather than once each, because each pass can
	// expose a character the other strips. A Unicode space is not in the
	// ". " cutset, so it shields the dots behind it, and stopping after one
	// pass yields ".." from a segment of nothing but dots and spaces.
	clean := sanitizeValue(segment)
	for {
		trimmed := strings.TrimSpace(strings.TrimRight(clean, ". "))
		if trimmed == clean {
			break
		}
		clean = trimmed
	}
	if clean == "" {
		return "", ErrEmptySegment
	}
	// Neither survives the trim above, which cannot leave a trailing dot.
	// They are refused by name anyway, because a component that walks the
	// tree is the one failure this function exists to prevent and it must
	// not depend on another line staying correct.
	if clean == "." || clean == ".." {
		return "", ErrEmptySegment
	}

	stem, _, _ := strings.Cut(clean, ".")
	if _, reserved := reservedNames[strings.ToLower(stem)]; reserved {
		return replacement + clean, nil
	}

	// A channel whose display name matches a library directory would
	// otherwise file recordings into the state directory or the capture
	// directory. The sidecar beside them would land on the ownership
	// marker. The display name arrives from platform metadata, so a
	// streamer picks that value, not the operator.
	if _, reserved := reservedSegments[strings.ToLower(clean)]; reserved {
		return replacement + clean, nil
	}

	// The list above names the device forms this package knows. The
	// standard library knows more of them, including the superscript digit
	// spellings Windows also treats as devices, and it is what the two
	// layers downstream enforce. A component this accepts and they refuse
	// is not contained anywhere: it is a recording that fails to move on
	// every sweep, forever, with a raw filesystem error and no reason
	// attached. Deferring here makes one function the definition instead
	// of two lists that drift.
	if !filepath.IsLocal(clean) {
		return replacement + clean, nil
	}
	return clean, nil
}

// Candidates yields every name relPath may take, the rendered name first
// and then " (2)", " (3)", and so on before the extension.
//
// Deduplicate picks a free one and a caller recovering an interrupted move
// searches the same series for the name it landed on. Both need the order to
// agree, so the sequence is defined once here rather than at each of them.
func Candidates(relPath string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if !yield(relPath) {
			return
		}

		dir := filepath.Dir(relPath)
		base := filepath.Base(relPath)
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)

		for n := 2; n <= maxDuplicates; n++ {
			if !yield(filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))) {
				return
			}
		}
	}
}

// Deduplicate returns the first variant of relPath that taken reports as
// free, appending " (2)", " (3)", and so on before the extension.
//
// Two broadcasts can legitimately share a channel, date, and title, so a
// collision is a naming problem rather than an error.
func Deduplicate(relPath string, taken func(string) bool) (string, error) {
	for candidate := range Candidates(relPath) {
		if !taken(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for %s after %d attempts", relPath, maxDuplicates)
}
