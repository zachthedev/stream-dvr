package naming

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Round trip
// ///////////////////////////////////////////////

func TestMatch_ReadsBackWhatRenderWrote(t *testing.T) {
	// The two directions come from one placeholder table, so every template
	// that renders a name has to read that name back. A template that can
	// write a path nothing can read is a recording this package files and
	// cannot account for.
	loc := time.FixedZone("test", -5*60*60)
	started := time.Date(2026, 8, 15, 18, 34, 0, 0, loc)

	tests := []struct {
		name     string
		template string
		fields   Fields
	}{
		{
			name:     "the default template",
			template: DefaultTemplate,
			fields: Fields{
				Platform: "twitch", Channel: "atrioc", Author: "atrioc",
				Title: "GET SMARTER SATURDAYS", StartedAt: started, Extension: "mkv",
			},
		},
		{
			name:     "platform first",
			template: "{platform}/{channel}/{date}-{title}.{ext}",
			fields: Fields{
				Platform: "twitch", Channel: "atrioc", Author: "atrioc",
				Title: "movie night", StartedAt: started, Extension: "mp4",
			},
		},
		{
			name:     "no title at all",
			template: "{channel}/{date} {time}.{ext}",
			fields: Fields{
				Channel: "atrioc", Author: "atrioc",
				StartedAt: started, Extension: "mkv",
			},
		},
		{
			name:     "category in the path",
			template: "{category}/{channel}/{date} - {title}.{ext}",
			fields: Fields{
				Channel: "atrioc", Author: "atrioc", Category: "Just Chatting",
				Title: "movie night", StartedAt: started, Extension: "mkv",
			},
		},
		{
			name:     "split date parts",
			template: "{year}/{month}/{day}/{channel} - {title}.{ext}",
			fields: Fields{
				Channel: "atrioc", Author: "atrioc", Title: "movie night",
				StartedAt: started, Extension: "mkv",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := mustParse(t, tt.template)

			rendered, err := tmpl.Render(tt.fields)
			if err != nil {
				t.Fatalf("Render() err = %v, want nil", err)
			}

			got, err := tmpl.Match(rendered.Path, loc)
			if err != nil {
				t.Fatalf("Match(%q) err = %v, want nil", rendered.Path, err)
			}

			// Only the fields this template carries come back. A template
			// that never writes the platform cannot be expected to name it.
			for placeholder, want := range map[string]string{
				"platform": tt.fields.Platform,
				"channel":  tt.fields.Channel,
				"author":   tt.fields.Author,
				"title":    tt.fields.Title,
				"category": tt.fields.Category,
				"ext":      tt.fields.Extension,
			} {
				if !tmpl.Uses(placeholder) {
					continue
				}
				if in := fieldValue(got.Fields, placeholder); in != want {
					t.Errorf("Match() %s = %q, want %q", placeholder, in, want)
				}
			}

			if !got.Fields.StartedAt.Equal(startedWanted(tmpl, started)) {
				t.Errorf("Match() StartedAt = %s, want %s",
					got.Fields.StartedAt, startedWanted(tmpl, started))
			}
			if got.Duplicate != 0 {
				t.Errorf("Match() Duplicate = %d, want 0", got.Duplicate)
			}
			if len(got.Lossy) != 0 {
				t.Errorf("Match() Lossy = %v, want nothing", got.Lossy)
			}
		})
	}
}

// fieldValue reads one placeholder's value out of Fields.
func fieldValue(f Fields, placeholder string) string {
	return placeholders[placeholder].render(f)
}

// startedWanted returns the start a template can carry back, which is only
// as precise as the parts it writes.
func startedWanted(tmpl *Template, started time.Time) time.Time {
	if !tmpl.Uses("time") {
		return time.Date(started.Year(), started.Month(), started.Day(),
			0, 0, 0, 0, started.Location())
	}
	return started
}

// ///////////////////////////////////////////////
// Refusals
// ///////////////////////////////////////////////

func TestMatch_RefusesWhatTheTemplateCannotHaveWritten(t *testing.T) {
	tests := []struct {
		name     string
		template string
		path     string
		want     error
		why      string
	}{
		{
			name:     "a date that is not a date",
			template: DefaultTemplate,
			path:     "atrioc/2026/atrioc - notadate 18-34 - night.mkv",
			want:     ErrNoMatch,
			why:      "the date has a shape and this is not it",
		},
		{
			name:     "an hour that does not exist",
			template: DefaultTemplate,
			path:     "atrioc/2026/atrioc - 2026-08-15 99-99 - night.mkv",
			want:     ErrNoMatch,
			why:      "99 is not an hour",
		},
		{
			name:     "a day that does not exist, written as one date",
			template: DefaultTemplate,
			path:     "atrioc/2026/atrioc - 2026-02-31 18-34 - night.mkv",
			want:     ErrNoMatch,
			why:      "February has no 31st",
		},
		{
			name:     "a day that does not exist, written in parts",
			template: "{year}/{month}/{day}/{channel} - {title}.{ext}",
			path:     "2026/02/31/atrioc - night.mkv",
			want:     ErrNoMatch,
			why: "split parts are read as numbers with no calendar behind them, so " +
				"nothing but the normalization check stops the 31st of February " +
				"becoming the 3rd of March and moving the recording a week",
		},
		{
			name:     "the same placeholder disagreeing with itself",
			template: DefaultTemplate,
			path:     "atrioc/2026/somebodyelse - 2026-08-15 18-34 - night.mkv",
			want:     ErrNoMatch,
			why:      "one template wrote both authors, so a path where they differ is not from it",
		},
		{
			name:     "a year that contradicts the date",
			template: "{year}/{channel} - {date} - {title}.{ext}",
			path:     "2025/atrioc - 2026-08-15 - night.mkv",
			want:     ErrConflictingDate,
			why:      "the recording cannot be in both years",
		},
		{
			name:     "a free field that could swallow its delimiter",
			template: "{channel} - {title}.{ext}",
			path:     "a - b - c.mkv",
			want:     ErrAmbiguousMatch,
			why:      "the channel could be 'a' or 'a - b', and picking either files a guess",
		},
		{
			name:     "a path from some other tool",
			template: DefaultTemplate,
			path:     "downloads/whatever.mkv",
			want:     ErrNoMatch,
			why:      "nothing about it fits",
		},
		{
			name:     "a value that climbed out of its component",
			template: "{channel}/{title}.{ext}",
			path:     "atrioc/nested/night.mkv",
			want:     ErrNoMatch,
			why:      "a rendered value holds no separator, so it cannot span two directories",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mustParse(t, tt.template).Match(tt.path, time.UTC)

			if !errors.Is(err, tt.want) {
				t.Fatalf("Match(%q) err = %v, want %v, because %s", tt.path, err, tt.want, tt.why)
			}
			// A refusal must not also hand back fields, or a caller that
			// checks the error second stores a reading that was rejected.
			if got.Fields != (Fields{}) || got.Duplicate != 0 {
				t.Errorf("Match(%q) = %+v alongside its refusal, want the zero match", tt.path, got)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Deduplication suffix
// ///////////////////////////////////////////////

func TestMatch_TakesTheDeduplicationSuffixOffTheTitle(t *testing.T) {
	// Candidates appends " (2)" after the name is rendered, so it belongs to
	// the series rather than to the broadcast. Left on, it reaches the
	// library as part of a title nobody typed.
	tests := []struct {
		name      string
		path      string
		wantTitle string
		wantDup   int
	}{
		{
			name:      "no suffix",
			path:      "atrioc/2026/atrioc - 2026-08-15 18-34 - night.mkv",
			wantTitle: "night",
			wantDup:   0,
		},
		{
			name:      "the second copy",
			path:      "atrioc/2026/atrioc - 2026-08-15 18-34 - night (2).mkv",
			wantTitle: "night",
			wantDup:   2,
		},
		{
			name:      "the last copy the series can reach",
			path:      "atrioc/2026/atrioc - 2026-08-15 18-34 - night (999).mkv",
			wantTitle: "night",
			wantDup:   999,
		},
		{
			name: "a number the series never writes",
			path: "atrioc/2026/atrioc - 2026-08-15 18-34 - night (1).mkv",
			// Candidates starts at 2, so " (1)" was somebody's title.
			wantTitle: "night (1)",
			wantDup:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mustParse(t, DefaultTemplate).Match(tt.path, time.UTC)
			if err != nil {
				t.Fatalf("Match(%q) err = %v, want nil", tt.path, err)
			}
			if got.Fields.Title != tt.wantTitle {
				t.Errorf("Match(%q) title = %q, want %q", tt.path, got.Fields.Title, tt.wantTitle)
			}
			if got.Duplicate != tt.wantDup {
				t.Errorf("Match(%q) Duplicate = %d, want %d", tt.path, got.Duplicate, tt.wantDup)
			}
		})
	}
}

func TestMatch_EveryNameCandidatesWritesReadsBack(t *testing.T) {
	// Candidates defines the series and this reads it, so the two have to
	// agree across the whole range rather than at the one suffix a test
	// happened to pick.
	tmpl := mustParse(t, DefaultTemplate)
	base := "atrioc/2026/atrioc - 2026-08-15 18-34 - night.mkv"

	seen := 0
	for candidate := range Candidates(base) {
		got, err := tmpl.Match(candidate, time.UTC)
		if err != nil {
			t.Fatalf("Match(%q) err = %v, want nil", candidate, err)
		}
		if got.Fields.Title != "night" {
			t.Errorf("Match(%q) title = %q, want %q", candidate, got.Fields.Title, "night")
		}
		if seen++; seen > maxDuplicates {
			break
		}
	}
	if seen != maxDuplicates {
		t.Errorf("walked %d candidates, want %d", seen, maxDuplicates)
	}
}

// ///////////////////////////////////////////////
// What a name cannot carry back
// ///////////////////////////////////////////////

func TestMatch_NamesTheFieldsItCannotRestore(t *testing.T) {
	// sanitizeValue maps every illegal character and every control
	// character onto one replacement, so the rendering is not reversible. A
	// caller storing one of these is storing a guess, and has to be told
	// which ones.
	tests := []struct {
		name  string
		title string
		want  []string
	}{
		{name: "ordinary text restores", title: "movie night", want: nil},
		{name: "a colon does not", title: "movie: night", want: []string{"title"}},
		{name: "a slash does not", title: "and/or", want: []string{"title"}},
		{name: "a control character does not", title: "movie\tnight", want: []string{"title"}},
	}

	tmpl := mustParse(t, DefaultTemplate)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered, err := tmpl.Render(Fields{
				Channel: "atrioc", Author: "atrioc", Title: tt.title,
				StartedAt: time.Date(2026, 8, 15, 18, 34, 0, 0, time.UTC),
				Extension: "mkv",
			})
			if err != nil {
				t.Fatalf("Render() err = %v, want nil", err)
			}

			got, err := tmpl.Match(rendered.Path, time.UTC)
			if err != nil {
				t.Fatalf("Match(%q) err = %v, want nil", rendered.Path, err)
			}
			if !slices.Equal(got.Lossy, tt.want) {
				t.Errorf("Match(%q) Lossy = %v, want %v", rendered.Path, got.Lossy, tt.want)
			}
			// A lossy field still comes back, because the sanitized form is
			// what the file is actually called.
			if got.Fields.Title == "" {
				t.Error("Match() title = \"\", want the rendered form")
			}
		})
	}
}

func TestMatch_LeavesAnUndatedTemplateUndated(t *testing.T) {
	// A template naming only the year leaves the month and the day to be
	// chosen, and the first of January is a day nobody recorded on. The
	// caller can say a recording is undated; it cannot unpick a date this
	// invented.
	got, err := mustParse(t, "{year}/{channel} - {title}.{ext}").
		Match("2026/atrioc - night.mkv", time.UTC)
	if err != nil {
		t.Fatalf("Match() err = %v, want nil", err)
	}
	if !got.Fields.StartedAt.IsZero() {
		t.Errorf("Match() StartedAt = %s, want the zero time", got.Fields.StartedAt)
	}
}

// ///////////////////////////////////////////////
// Separators and zones
// ///////////////////////////////////////////////

func TestMatch_ReadsEitherSeparator(t *testing.T) {
	// Render joins with the host separator, and a library is read on
	// another operating system than the one that wrote it.
	tmpl := mustParse(t, DefaultTemplate)
	for _, path := range []string{
		"atrioc/2026/atrioc - 2026-08-15 18-34 - night.mkv",
		`atrioc\2026\atrioc - 2026-08-15 18-34 - night.mkv`,
		filepath.Join("atrioc", "2026", "atrioc - 2026-08-15 18-34 - night.mkv"),
	} {
		got, err := tmpl.Match(path, time.UTC)
		if err != nil {
			t.Fatalf("Match(%q) err = %v, want nil", path, err)
		}
		if got.Fields.Title != "night" {
			t.Errorf("Match(%q) title = %q, want %q", path, got.Fields.Title, "night")
		}
	}
}

func TestMatch_ReadsTheClockInTheGivenZone(t *testing.T) {
	// Render formats in whichever location the operator configured, so a
	// wall clock read in another zone moves the recording to a different
	// instant and sometimes a different day.
	tmpl := mustParse(t, DefaultTemplate)
	path := "atrioc/2026/atrioc - 2026-08-15 18-34 - night.mkv"

	east := time.FixedZone("east", 5*60*60)
	west := time.FixedZone("west", -5*60*60)

	inEast, err := tmpl.Match(path, east)
	if err != nil {
		t.Fatalf("Match() err = %v, want nil", err)
	}
	inWest, err := tmpl.Match(path, west)
	if err != nil {
		t.Fatalf("Match() err = %v, want nil", err)
	}

	if inEast.Fields.StartedAt.Equal(inWest.Fields.StartedAt) {
		t.Error("Match() gave one instant for two zones, want the zone to move it")
	}
	// The wall clock is what the name states, and it reads the same either
	// way. Only the instant behind it moves.
	for _, got := range []Match{inEast, inWest} {
		if got.Fields.StartedAt.Hour() != 18 || got.Fields.StartedAt.Minute() != 34 {
			t.Errorf("Match() clock = %02d-%02d, want 18-34",
				got.Fields.StartedAt.Hour(), got.Fields.StartedAt.Minute())
		}
	}
}

func TestMatch_DefaultsToUTCRatherThanTheMachineZone(t *testing.T) {
	// A nil location must not silently mean Local. A library read on a
	// machine in another zone would date every recording differently from
	// the machine that wrote it.
	got, err := mustParse(t, DefaultTemplate).
		Match("atrioc/2026/atrioc - 2026-08-15 18-34 - night.mkv", nil)
	if err != nil {
		t.Fatalf("Match() err = %v, want nil", err)
	}
	if name, _ := got.Fields.StartedAt.Zone(); name != "UTC" {
		t.Errorf("Match() zone = %q, want UTC", name)
	}
}

// ///////////////////////////////////////////////
// Values somebody else chooses
// ///////////////////////////////////////////////

func TestMatch_ReadsBackNamesBuiltFromHostileMetadata(t *testing.T) {
	// The author and the title both arrive from the platform, so a streamer
	// picks them. A name this package writes and cannot read is a recording
	// it files and can never account for again, and the operator finds out
	// when a lost database cannot be rebuilt.
	loc := time.UTC
	started := time.Date(2026, 8, 15, 18, 34, 0, 0, loc)

	tests := []struct {
		name   string
		author string
		title  string
		why    string
	}{
		{
			name:   "a title shaped like the separators around it",
			author: "atrioc",
			title:  "X - 2026-08-15 18-34 - Y",
			why:    "the later author can swallow the separators, and one pass cannot give the earlier reading back",
		},
		{
			name:   "a title holding another whole rendered name",
			author: "atrioc",
			title:  "atrioc - 2026-01-01 00-00 - something else",
			why:    "it reads as a second author and date",
		},
		{
			name:   "a title ending in the deduplication shape",
			author: "atrioc",
			title:  "night (2)",
			why:    "the suffix belongs to the series, and a title may end that way on its own",
		},
		{
			name:   "a title of nothing but separators",
			author: "atrioc",
			title:  " - - - ",
			why:    "every literal in the template appears in the value",
		},
		{
			name:   "an author holding a separator",
			author: "a - b",
			title:  "night",
			why:    "the author is free text and the separator is not reserved to the template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := mustParse(t, DefaultTemplate)
			fields := Fields{
				Channel: "atrioc", Author: tt.author, Title: tt.title,
				StartedAt: started, Extension: "mkv",
			}

			rendered, err := tmpl.Render(fields)
			if err != nil {
				t.Skipf("Render() refused this metadata outright: %v", err)
			}

			got, err := tmpl.Match(rendered.Path, loc)
			if err != nil {
				t.Fatalf("Match(%q) err = %v, want it read back, because %s",
					rendered.Path, err, tt.why)
			}
			if got.Fields.Author != tt.author {
				t.Errorf("Match() author = %q, want %q", got.Fields.Author, tt.author)
			}
			if !got.Fields.StartedAt.Equal(started) {
				t.Errorf("Match() started = %s, want %s", got.Fields.StartedAt, started)
			}
		})
	}
}

func TestMatch_ReadsBackAValueTheSegmentRulesRewrote(t *testing.T) {
	// SanitizeSegment rewrites a whole path component, so one value renders
	// two ways in one path: an author called CON is prefixed in its own
	// directory and left alone inside the longer filename beside it.
	//
	// Both spellings are the same value, and the embedded one kept it. A
	// refusal here would make every recording of such a channel unreadable
	// by the program that named it, which is the failure this package exists
	// to prevent.
	loc := time.UTC
	started := time.Date(2026, 8, 15, 18, 34, 0, 0, loc)
	tmpl := mustParse(t, DefaultTemplate)

	tests := []struct {
		author string
		why    string
	}{
		{author: "CON", why: "a Windows device name, prefixed as a directory"},
		{author: "nul", why: "the same, lowercase"},
		{author: "lpt1", why: "a numbered device"},
		{author: ".dvr", why: "the library's own state directory"},
		{author: "incoming", why: "the library's own capture directory"},
		{author: "streamer.", why: "a trailing dot, which Windows strips from a component"},
	}

	for _, tt := range tests {
		t.Run(tt.author, func(t *testing.T) {
			rendered, err := tmpl.Render(Fields{
				Channel: "atrioc", Author: tt.author, Title: "night",
				StartedAt: started, Extension: "mkv",
			})
			if err != nil {
				t.Fatalf("Render(%q) err = %v, want nil", tt.author, err)
			}

			got, err := tmpl.Match(rendered.Path, loc)
			if err != nil {
				t.Fatalf("Match(%q) err = %v, want it read back: %s is %s",
					rendered.Path, err, tt.author, tt.why)
			}
			if got.Fields.Author != tt.author {
				t.Errorf("Match(%q) author = %q, want %q",
					rendered.Path, got.Fields.Author, tt.author)
			}
			if got.Fields.Title != "night" {
				t.Errorf("Match(%q) title = %q, want %q", rendered.Path, got.Fields.Title, "night")
			}
		})
	}
}

func TestReconcile_RefusesTwoValuesThatAreNotOne(t *testing.T) {
	// The rewrite is accepted in one direction only. Any two readings that
	// are not the same value have to stay a refusal, or a path naming two
	// different authors reads as one.
	tests := []struct {
		name  string
		a, b  string
		want  string
		agree bool
	}{
		{name: "identical", a: "atrioc", b: "atrioc", want: "atrioc", agree: true},
		{name: "prefixed as a directory", a: "_CON", b: "CON", want: "CON", agree: true},
		{name: "prefixed the other way round", a: "CON", b: "_CON", want: "CON", agree: true},
		{name: "trimmed as a directory", a: "streamer", b: "streamer.", want: "streamer.", agree: true},
		{name: "two different channels", a: "atrioc", b: "somebodyelse", agree: false},
		{name: "a prefix that is not the rule", a: "_atrioc", b: "atrioc", agree: false},
		{name: "one is a substring", a: "atrio", b: "atrioc", agree: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, agreed := reconcile(tt.a, tt.b)

			if agreed != tt.agree {
				t.Fatalf("reconcile(%q, %q) agreed = %t, want %t", tt.a, tt.b, agreed, tt.agree)
			}
			if agreed && got != tt.want {
				t.Errorf("reconcile(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Template bounds
// ///////////////////////////////////////////////

func TestParse_RefusesATemplateTooLargeToReadBack(t *testing.T) {
	// Reading a name back costs roughly the cube of the capture count. A
	// template is operator config and nothing else checks it, so past the
	// bound one filename takes seconds and the import cannot interrupt
	// itself mid-file.
	raw := strings.Repeat("{title}", maxPlaceholders+1) + ".{ext}"

	if _, err := Parse(raw); !errors.Is(err, ErrTooManyPlaceholders) {
		t.Errorf("Parse() err = %v, want ErrTooManyPlaceholders", err)
	}
	// The bound itself still parses, so it is a limit rather than an
	// off-by-one that refuses a template it names as allowed.
	atLimit := strings.Repeat("{title}", maxPlaceholders-1) + ".{ext}"
	if _, err := Parse(atLimit); err != nil {
		t.Errorf("Parse() err = %v at %d placeholders, want nil", err, maxPlaceholders)
	}
}

func TestMatch_RefusesAValueRenderCouldNotHaveWritten(t *testing.T) {
	// sanitizeValue replaces every control character, so a rendered name
	// holds none. Reading one back would claim this package wrote a name it
	// cannot write, and the value would then be stored as a title.
	tmpl := mustParse(t, DefaultTemplate)

	for _, title := range []string{
		"a\x1b[31mred\x1b[0m",
		"a\nb",
		"a\x00b",
		"a\x7fb",
	} {
		path := "atrioc/2026/atrioc - 2026-08-15 18-34 - " + title + ".mkv"
		if _, err := tmpl.Match(path, time.UTC); !errors.Is(err, ErrNoMatch) {
			t.Errorf("Match(%q) err = %v, want ErrNoMatch", path, err)
		}
	}
}

func TestMatch_RefusesAWallClockTheZoneSkips(t *testing.T) {
	// America/New_York jumps from 01:59 to 03:00 on 10 March 2024, so 02:30
	// never happens. time.Date normalizes it to 01:30 and the calendar check
	// passes, which would move the recording an hour with nothing about the
	// name changing. Render cannot write one, so the name is not from here.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}

	path := "atrioc/2024/atrioc - 2024-03-10 02-30 - night.mkv"
	if _, err := mustParse(t, DefaultTemplate).Match(path, loc); !errors.Is(err, ErrNoMatch) {
		t.Errorf("Match(%q) err = %v, want ErrNoMatch", path, err)
	}

	// An hour on either side of the gap is ordinary and still reads.
	for _, clock := range []string{"01-30", "03-30"} {
		ok := "atrioc/2024/atrioc - 2024-03-10 " + clock + " - night.mkv"
		if _, err := mustParse(t, DefaultTemplate).Match(ok, loc); err != nil {
			t.Errorf("Match(%q) err = %v, want nil", ok, err)
		}
	}
}

func TestMatch_LeavesAPaddedNumberOnTheTitle(t *testing.T) {
	// Candidates formats the number plainly and never pads it, so a padded
	// one is somebody's own title. Reading it as a suffix takes characters
	// off a name nobody chose to shorten.
	tests := []struct {
		path      string
		wantTitle string
		wantDup   int
	}{
		{
			path:      "atrioc/2026/atrioc - 2026-08-15 18-34 - night (007).mkv",
			wantTitle: "night (007)",
			wantDup:   0,
		},
		{
			path:      "atrioc/2026/atrioc - 2026-08-15 18-34 - night (07).mkv",
			wantTitle: "night (07)",
			wantDup:   0,
		},
		{
			path:      "atrioc/2026/atrioc - 2026-08-15 18-34 - night (7).mkv",
			wantTitle: "night",
			wantDup:   7,
		},
	}

	for _, tt := range tests {
		got, err := mustParse(t, DefaultTemplate).Match(tt.path, time.UTC)
		if err != nil {
			t.Fatalf("Match(%q) err = %v, want nil", tt.path, err)
		}
		if got.Fields.Title != tt.wantTitle {
			t.Errorf("Match(%q) title = %q, want %q", tt.path, got.Fields.Title, tt.wantTitle)
		}
		if got.Duplicate != tt.wantDup {
			t.Errorf("Match(%q) Duplicate = %d, want %d", tt.path, got.Duplicate, tt.wantDup)
		}
	}
}
