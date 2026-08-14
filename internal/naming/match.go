package naming

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Match is what a rendered path yields when it is read back.
//
// It is a reading of a name, not a record of a recording. Rendering discards
// information that no reading can restore, and the caller has to treat every
// field here as a claim the file's own contents may contradict. Lossy names
// which fields those are.
type Match struct {
	// Fields are the values the name carries. StartedAt is set only when
	// the template dates the recording, and carries whatever precision the
	// template asked for: a template with {date} but no {time} yields
	// midnight, not an unknown hour.
	Fields Fields
	// Duplicate is the deduplication suffix the name carried, or zero. A
	// name ending " (2)" is the second recording to render one name, and
	// the digit belongs to Candidates rather than to the title.
	Duplicate int
	// Lossy names the fields whose rendering cannot be undone, sorted. A
	// caller that stores one of these is storing a guess.
	Lossy []string
}

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

var (
	// ErrNoMatch reports a path the template cannot have produced.
	ErrNoMatch = errors.New("path does not fit the naming template")

	// ErrAmbiguousMatch reports a path the template could have produced
	// from more than one set of fields.
	//
	// It is an error rather than a choice. Two readings of one name are two
	// different broadcasts, and picking either one files a recording under
	// a title nobody can check.
	ErrAmbiguousMatch = errors.New("path fits the naming template more than one way")

	// ErrConflictingDate reports a template dating one recording twice,
	// with the two dates disagreeing.
	ErrConflictingDate = errors.New("path carries two different dates")
)

// ///////////////////////////////////////////////
// Deduplication suffix
// ///////////////////////////////////////////////

// duplicateSuffix matches the " (2)" Candidates appends before the
// extension.
//
// The digits are bounded the same way maxDuplicates bounds the series that
// writes them, and a leading zero is refused because Candidates formats the
// number plainly and never pads it. A title ending " (007)" is somebody's
// own, and reading it as the seventh copy takes four characters off a name
// nobody chose to shorten.
var duplicateSuffix = regexp.MustCompile(`^(.*) \(([1-9]\d{0,2})\)$`)

// ///////////////////////////////////////////////
// Matching
// ///////////////////////////////////////////////

// Match reads a rendered path back into the fields that produced it.
//
// It is the inverse of Render, as far as one exists. Both directions come
// from the same placeholder table, so a template that renders a name is the
// template that reads it, and neither can drift from the other.
//
// Times are assembled in loc, because Render formats them in whichever
// location the operator configured. Reading a wall clock in any other zone
// moves the recording to a different day.
//
// Returns ErrNoMatch, ErrAmbiguousMatch, or ErrConflictingDate. A caller
// that gets fields back still has to check them against the file: see Lossy.
func (t *Template) Match(relPath string, loc *time.Location) (Match, error) {
	if loc == nil {
		loc = time.UTC
	}

	cleaned, duplicate := splitDuplicate(toSlash(relPath))

	// Greedy and lazy readings of the same name, compared rather than
	// chosen between. Where a free-text value can absorb the literal that
	// follows it, the two disagree, and that disagreement is the whole
	// ambiguity test: an unambiguous name reads the same either way.
	greedy, ok := t.capture(cleaned, false)
	if !ok {
		return Match{}, fmt.Errorf("%q: %w", relPath, ErrNoMatch)
	}
	lazy, ok := t.capture(cleaned, true)
	if !ok || !sameValues(greedy, lazy) {
		return Match{}, fmt.Errorf("%q: %w", relPath, ErrAmbiguousMatch)
	}

	fields, err := fieldsFrom(greedy, loc)
	if err != nil {
		return Match{}, fmt.Errorf("%q: %w", relPath, err)
	}
	return Match{Fields: fields, Duplicate: duplicate, Lossy: lossyFields(greedy)}, nil
}

// splitDuplicate removes a deduplication suffix from a path's stem,
// reporting which one it carried.
//
// The suffix sits on the stem rather than after the extension, because that
// is where Candidates puts it. A title that genuinely ends in " (2)" reads
// the same as a second copy and loses the suffix here, which is one of the
// reasons a name is not a record.
func splitDuplicate(relPath string) (string, int) {
	dir, base := path.Split(relPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	groups := duplicateSuffix.FindStringSubmatch(stem)
	if groups == nil {
		return relPath, 0
	}
	n, err := strconv.Atoi(groups[2])
	if err != nil || n < 2 || n > maxDuplicates {
		return relPath, 0
	}
	return dir + groups[1] + ext, n
}

// capture runs the template's pattern over a path, returning each
// placeholder's value.
//
// lazy selects the non-greedy reading of every free-text placeholder. The
// two readings exist to be compared: see Match.
func (t *Template) capture(relPath string, lazy bool) (map[string]string, bool) {
	groups, ok := t.run(t.expression(lazy), relPath)
	if !ok {
		return nil, false
	}
	if values, agreed := t.collapse(groups); agreed {
		return values, true
	}

	// A placeholder the template uses twice disagreed with itself. The whole
	// name is read again with the earliest occurrence pinned as a literal,
	// because that one is bounded by the literals of its own path component
	// and the later one is not.
	//
	// A title shaped like the separators around it is what makes this happen,
	// and a streamer picks the title. Without the second pass the later
	// occurrence swallows the separators, the two readings differ, and a name
	// with one correct parse is refused: the recording it belongs to then
	// cannot be imported at all, which is the one job this exists for.
	pinned, err := regexp.Compile(t.pattern(lazy, t.repeated(groups)))
	if err != nil {
		return nil, false
	}
	groups, ok = t.run(pinned, relPath)
	if !ok {
		return nil, false
	}
	values, agreed := t.collapse(groups)
	if !agreed {
		return nil, false
	}
	return values, true
}

// run matches a path, returning one captured value per placeholder
// occurrence.
func (t *Template) run(expression *regexp.Regexp, relPath string) ([]string, bool) {
	groups := expression.FindStringSubmatch(relPath)
	if groups == nil {
		return nil, false
	}
	return groups[1:], true
}

// collapse folds per-occurrence captures into one value per placeholder,
// reporting whether every repeated placeholder agreed with itself.
func (t *Template) collapse(groups []string) (map[string]string, bool) {
	values := make(map[string]string, len(groups))
	for i, name := range t.names() {
		seen, repeat := values[name]
		if !repeat {
			values[name] = groups[i]
			continue
		}
		agreed, consistent := reconcile(seen, groups[i])
		if !consistent {
			return nil, false
		}
		values[name] = agreed
	}
	return values, true
}

// reconcile reports the value two readings of one placeholder agree on.
//
// Equality is the ordinary answer. The other case is a value that renders two
// ways in one path: SanitizeSegment rewrites a whole path component, so a
// value standing alone as a directory is rewritten while the same value
// inside a longer filename is left as it was. An author called CON becomes
// _CON as a directory and stays CON in the file beside it, and a channel
// named with a trailing dot loses it in the directory only.
//
// Both are one value, and the embedded reading is the one that kept it, so
// that is what comes back. Refusing here instead would make every recording
// of such a channel unreadable by the program that named it.
func reconcile(a, b string) (string, bool) {
	switch {
	case a == b:
		return a, true
	case segmentForm(b) == a:
		return b, true
	case segmentForm(a) == b:
		return a, true
	}
	return "", false
}

// segmentForm is what a value becomes when it stands alone as a path
// component. A value SanitizeSegment refuses outright is returned unchanged,
// because it never reached a path for this to be read back from.
func segmentForm(value string) string {
	clean, err := SanitizeSegment(value)
	if err != nil {
		return value
	}
	return clean
}

// repeated returns the earliest captured value of every placeholder the
// template names more than once.
//
// Only the repeated ones. Pinning a placeholder that appears once holds it to
// whatever the first pass happened to read, and the first pass is the one
// that read it wrongly: the title is the value a greedy author took from, so
// pinning it would fix the very reading the second pass exists to correct.
func (t *Template) repeated(groups []string) map[string]string {
	names := t.names()
	seen := make(map[string]int, len(names))
	for _, name := range names {
		seen[name]++
	}

	pins := make(map[string]string, len(names))
	for i, name := range names {
		if seen[name] < 2 {
			continue
		}
		if _, held := pins[name]; !held {
			pins[name] = groups[i]
		}
	}
	return pins
}

// names lists the placeholder each capture group holds, in order.
func (t *Template) names() []string {
	names := make([]string, 0, len(t.parts))
	for _, p := range t.parts {
		if p.placeholder != "" {
			names = append(names, p.placeholder)
		}
	}
	return names
}

// expression returns the matcher built when the template was parsed.
func (t *Template) expression(lazy bool) *regexp.Regexp {
	if lazy {
		return t.lazy
	}
	return t.greedy
}

// compile builds the matcher for this template, with nothing pinned.
func (t *Template) compile(lazy bool) *regexp.Regexp {
	// Parse validated every placeholder against the table, and each pattern
	// there is a literal this package wrote, so the result always compiles.
	return regexp.MustCompile(t.pattern(lazy, nil))
}

// pattern builds the expression matching this template.
//
// A placeholder named in pinned matches that exact value rather than its own
// shape, which is how a repeated placeholder is held to one reading. RE2 has
// no backreference, so agreement is expressed by substituting the value.
func (t *Template) pattern(lazy bool, pinned map[string]string) string {
	var b strings.Builder
	b.WriteString(`\A`)
	for _, p := range t.parts {
		if p.placeholder == "" {
			b.WriteString(regexp.QuoteMeta(toSlash(p.literal)))
			continue
		}
		b.WriteString("(")
		if value, held := pinned[p.placeholder]; held {
			b.WriteString(regexp.QuoteMeta(value))
		} else {
			b.WriteString(placeholders[p.placeholder].pattern)
			if lazy && placeholders[p.placeholder].pattern == freeText {
				b.WriteString("?")
			}
		}
		b.WriteString(")")
	}
	b.WriteString(`\z`)
	return b.String()
}

// sameValues reports whether two readings of one name agree everywhere.
func sameValues(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if b[name] != value {
			return false
		}
	}
	return true
}

// fieldsFrom assembles Fields from captured values.
func fieldsFrom(values map[string]string, loc *time.Location) (Fields, error) {
	started, err := startedFrom(values, loc)
	if err != nil {
		return Fields{}, err
	}
	return Fields{
		Platform:  values["platform"],
		Channel:   values["channel"],
		Author:    values["author"],
		Title:     values["title"],
		Category:  values["category"],
		StartedAt: started,
		Extension: values["ext"],
	}, nil
}

// startedFrom assembles the start time from whichever date parts the
// template carried, or the zero time when it dates the recording partly or
// not at all.
//
// A partial date is no date. A template naming only the year leaves the
// month and the day to be chosen, and the first of January is a day nobody
// recorded on. The caller learns the recording is undated and can say so,
// which is what it cannot do with a date this function made up.
func startedFrom(values map[string]string, loc *time.Location) (time.Time, error) {
	parts, err := datePartsFrom(values)
	if err != nil {
		return time.Time{}, err
	}
	year, month, day := parts["year"], parts["month"], parts["day"]
	if year == 0 || month == 0 || day == 0 {
		return time.Time{}, nil
	}

	hour, minute := 0, 0
	if clock, ok := values["time"]; ok {
		parsed, err := time.Parse("15-04", clock)
		if err != nil {
			return time.Time{}, ErrNoMatch
		}
		hour, minute = parsed.Hour(), parsed.Minute()
	}

	started := time.Date(year, time.Month(month), day, hour, minute, 0, 0, loc)
	// time.Date normalizes an out of range value rather than refusing it, so
	// a 31st of February becomes the 3rd of March and the recording moves to
	// a day the name never named.
	//
	// The clock is checked as well as the calendar. A wall time inside the
	// hour a zone skips forward does not exist, and normalizing it moves the
	// recording an hour with nothing else about the name changing. Render
	// cannot write one, so a name carrying it was not written here.
	if started.Year() != year || int(started.Month()) != month || started.Day() != day ||
		started.Hour() != hour || started.Minute() != minute {
		return time.Time{}, ErrNoMatch
	}
	return started, nil
}

// datePartsFrom reads whichever calendar parts the template carried, keyed
// by placeholder name. A part the template does not name is absent rather
// than zero.
//
// A template may date a recording twice, with {date} and with {year} and its
// siblings. The two are checked against each other rather than one being
// preferred, because a path where they disagree was not rendered by this
// template, and reading it either way invents a broadcast.
func datePartsFrom(values map[string]string) (map[string]int, error) {
	parts := make(map[string]int, 3)

	if stamp, ok := values["date"]; ok {
		parsed, err := time.Parse("2006-01-02", stamp)
		if err != nil {
			return nil, ErrNoMatch
		}
		parts["year"], parts["month"], parts["day"] = parsed.Year(), int(parsed.Month()), parsed.Day()
	}

	for _, name := range []string{"year", "month", "day"} {
		text, ok := values[name]
		if !ok {
			continue
		}
		part, err := strconv.Atoi(text)
		if err != nil {
			return nil, ErrNoMatch
		}
		if seen, dated := parts[name]; dated && seen != part {
			return nil, ErrConflictingDate
		}
		parts[name] = part
	}
	return parts, nil
}

// lossyFields names the captured values whose rendering cannot be undone,
// sorted.
//
// A value holding the replacement character passed through sanitizeValue,
// which maps every illegal character and every control character onto that
// one byte. The original is not recoverable from the result, and two
// different originals reach the same rendering.
func lossyFields(values map[string]string) []string {
	var lossy []string
	for _, name := range []string{"author", "category", "channel", "platform", "title"} {
		if value, ok := values[name]; ok && strings.Contains(value, replacement) {
			lossy = append(lossy, name)
		}
	}
	return lossy
}
