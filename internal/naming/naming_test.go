package naming

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// startedAt is a fixed broadcast start, so date rendering is decidable.
var startedAt = time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

// completeFields returns fields with every value populated. Tests blank
// out the one field they are about.
func completeFields() Fields {
	return Fields{
		Platform:  "twitch",
		Channel:   "examplechannel",
		Author:    "ExampleChannel",
		Title:     "Midnight Build Stream",
		Category:  "Just Chatting",
		StartedAt: startedAt,
		Extension: "mkv",
	}
}

// mustParse parses a template or fails the test.
func mustParse(t *testing.T, raw string) *Template {
	t.Helper()

	tmpl, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) err = %v, want nil", raw, err)
	}
	return tmpl
}

// ///////////////////////////////////////////////
// Parse
// ///////////////////////////////////////////////

func TestParse_Valid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "default template", raw: DefaultTemplate},
		{name: "single placeholder", raw: "{title}"},
		{name: "adjacent placeholders", raw: "{author}{title}"},
		{name: "leading and trailing literals", raw: "vods/{author}/end"},
		{name: "every placeholder", raw: "{platform}{channel}{author}{title}{category}{date}{time}{year}{month}{day}{ext}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse(%q) err = %v, want nil", tt.raw, err)
			}
			if tmpl.String() != tt.raw {
				t.Errorf("String() = %q, want %q", tmpl.String(), tt.raw)
			}
		})
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{name: "empty", raw: "", wantErr: ErrEmptyTemplate},
		{name: "whitespace only", raw: "   ", wantErr: ErrEmptyTemplate},
		{name: "no placeholders", raw: "recording.mkv", wantErr: ErrNoPlaceholders},
		{name: "unclosed placeholder", raw: "{author} - {title", wantErr: ErrUnclosedPlaceholder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Parse(%q) err = %v, want it to wrap %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestParse_UnknownPlaceholder(t *testing.T) {
	_, err := Parse("{author} - {streamer}")

	var unknown *UnknownPlaceholderError
	if !errors.As(err, &unknown) {
		t.Fatalf("Parse() err = %v, want an *UnknownPlaceholderError", err)
	}
	if unknown.Placeholder != "streamer" {
		t.Errorf("Placeholder = %q, want %q", unknown.Placeholder, "streamer")
	}
	// The message must list the valid names, or a typo costs a docs trip.
	if !strings.Contains(unknown.Error(), "author") {
		t.Errorf("Error() = %q, want it to list valid placeholders", unknown.Error())
	}
}

func TestTemplate_Uses(t *testing.T) {
	tmpl := mustParse(t, "{author} - {title}")

	if !tmpl.Uses("author") {
		t.Error("Uses(author) = false, want true")
	}
	if tmpl.Uses("category") {
		t.Error("Uses(category) = true, want false")
	}
}

func TestPlaceholders_Sorted(t *testing.T) {
	got := Placeholders()
	if len(got) == 0 {
		t.Fatal("Placeholders() is empty")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("Placeholders() not sorted at %d: %q then %q", i, got[i-1], got[i])
		}
	}
}

// ///////////////////////////////////////////////
// Render, happy path
// ///////////////////////////////////////////////

func TestRender_DefaultTemplate(t *testing.T) {
	tmpl := mustParse(t, DefaultTemplate)

	got, err := tmpl.Render(completeFields())
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}

	want := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	if got.Path != want {
		t.Errorf("Render() path = %q, want %q", got.Path, want)
	}
	if len(got.Fallbacks) != 0 {
		t.Errorf("Render() fallbacks = %v, want none", got.Fallbacks)
	}
	if got.TitleShortened {
		t.Error("Render() reported the title shortened, want false")
	}
}

func TestRender_PlaceholderValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "platform", raw: "{platform}", want: "twitch"},
		{name: "channel", raw: "{channel}", want: "examplechannel"},
		{name: "author", raw: "{author}", want: "ExampleChannel"},
		{name: "category", raw: "{category}", want: "Just Chatting"},
		{name: "date", raw: "{date}", want: "2026-03-04"},
		{name: "time uses no colon", raw: "{time}", want: "21-15"},
		{name: "year", raw: "{year}", want: "2026"},
		{name: "month is zero padded", raw: "{month}", want: "03"},
		{name: "day is zero padded", raw: "{day}", want: "04"},
		{name: "ext drops a leading dot", raw: "{ext}", want: "mkv"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := completeFields()
			fields.Extension = ".mkv"

			got, err := mustParse(t, tt.raw).Render(fields)
			if err != nil {
				t.Fatalf("Render() err = %v, want nil", err)
			}
			if got.Path != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.raw, got.Path, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Render, the missing-field guard
// ///////////////////////////////////////////////

func TestRender_BlocksOnMissingFields(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Fields)
		wantMissing []string
	}{
		{
			name:        "no title",
			mutate:      func(f *Fields) { f.Title = "" },
			wantMissing: []string{"title"},
		},
		{
			name:        "whitespace-only title",
			mutate:      func(f *Fields) { f.Title = "   " },
			wantMissing: []string{"title"},
		},
		{
			name:        "no author and no channel to fall back to",
			mutate:      func(f *Fields) { f.Author, f.Channel = "", "" },
			wantMissing: []string{"author"},
		},
		{
			name:        "no extension",
			mutate:      func(f *Fields) { f.Extension = "" },
			wantMissing: []string{"ext"},
		},
		{
			name:        "several missing, reported together and sorted",
			mutate:      func(f *Fields) { f.Title, f.Author, f.Channel, f.Extension = "", "", "", "" },
			wantMissing: []string{"author", "ext", "title"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := completeFields()
			tt.mutate(&fields)

			_, err := mustParse(t, DefaultTemplate).Render(fields)

			var missing *MissingFieldError
			if !errors.As(err, &missing) {
				t.Fatalf("Render() err = %v, want a *MissingFieldError", err)
			}
			if strings.Join(missing.Placeholders, ",") != strings.Join(tt.wantMissing, ",") {
				t.Errorf("MissingFieldError.Placeholders = %v, want %v", missing.Placeholders, tt.wantMissing)
			}
		})
	}
}

func TestRender_ReproducesTheHeadlessNameBug(t *testing.T) {
	// A recorder that names files straight from a failed metadata call
	// produced " - 2026-03-04 21-15 - .ts": an empty author, an empty
	// title, and the separators still in place. Rendering must refuse
	// rather than emit that.
	fields := Fields{StartedAt: startedAt, Extension: "ts"}

	got, err := mustParse(t, DefaultTemplate).Render(fields)
	if err == nil {
		t.Fatalf("Render() with no metadata returned %q, want a refusal", got.Path)
	}

	var missing *MissingFieldError
	if !errors.As(err, &missing) {
		t.Fatalf("Render() err = %v, want a *MissingFieldError", err)
	}
	for _, want := range []string{"author", "title"} {
		if !strings.Contains(missing.Error(), want) {
			t.Errorf("MissingFieldError.Error() = %q, want it to name %q", missing.Error(), want)
		}
	}
}

func TestRender_OnlyChecksPlaceholdersTheTemplateUses(t *testing.T) {
	// An absent category cannot block a template that never mentions it.
	fields := completeFields()
	fields.Category = ""

	if _, err := mustParse(t, "{author} - {date}.{ext}").Render(fields); err != nil {
		t.Errorf("Render() err = %v, want nil", err)
	}
}

// ///////////////////////////////////////////////
// Author fallback
// ///////////////////////////////////////////////

func TestRender_AuthorFallsBackToChannel(t *testing.T) {
	fields := completeFields()
	fields.Author = ""

	got, err := mustParse(t, "{author} - {date}.{ext}").Render(fields)
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}
	if got.Path != "examplechannel - 2026-03-04.mkv" {
		t.Errorf("Render() = %q, want the channel login in place of the author", got.Path)
	}
	// The fallback must be visible, or a permanently broken display name
	// looks like a correct result.
	if len(got.Fallbacks) != 1 || got.Fallbacks[0] != "author" {
		t.Errorf("Render() fallbacks = %v, want [author]", got.Fallbacks)
	}
}

func TestRender_NoFallbackWhenAuthorIsPresent(t *testing.T) {
	got, err := mustParse(t, "{author}.{ext}").Render(completeFields())
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}
	if len(got.Fallbacks) != 0 {
		t.Errorf("Render() fallbacks = %v, want none", got.Fallbacks)
	}
}

func TestRender_AuthorMatchingChannelIsNotAFallback(t *testing.T) {
	// A channel whose display name equals its login is not a degraded
	// result, so it must not be reported as one.
	fields := completeFields()
	fields.Author = "examplechannel"

	got, err := mustParse(t, "{author}.{ext}").Render(fields)
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}
	if len(got.Fallbacks) != 0 {
		t.Errorf("Render() fallbacks = %v, want none", got.Fallbacks)
	}
}

// ///////////////////////////////////////////////
// Sanitization
// ///////////////////////////////////////////////

func TestSanitizeSegment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "forward slash in a title",
			in:   "Midnight Build Stream / Part Two",
			want: "Midnight Build Stream _ Part Two",
		},
		{name: "colon", in: "Part 1: the reckoning", want: "Part 1_ the reckoning"},
		{name: "every illegal character", in: `a<b>c:d"e/f\g|h?i*j`, want: "a_b_c_d_e_f_g_h_i_j"},
		{name: "control character", in: "line\x01break", want: "line_break"},
		{name: "trailing dot", in: "stream.", want: "stream"},
		{name: "trailing dots and spaces", in: "stream. . ", want: "stream"},
		{name: "leading and trailing whitespace", in: "  stream  ", want: "stream"},
		{name: "unicode is preserved", in: "配信 ✨", want: "配信 ✨"},
		{name: "reserved device name", in: "CON", want: "_CON"},
		{name: "reserved name with extension", in: "nul.mkv", want: "_nul.mkv"},
		{name: "reserved name lower case", in: "com1", want: "_com1"},
		{name: "not reserved despite prefix", in: "console", want: "console"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizeSegment(tt.in)
			if err != nil {
				t.Fatalf("SanitizeSegment(%q) err = %v, want nil", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("SanitizeSegment(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeSegment_EmptyResult(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "spaces only", in: "   "},
		{name: "dots only", in: "..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := SanitizeSegment(tt.in); !errors.Is(err, ErrEmptySegment) {
				t.Errorf("SanitizeSegment(%q) err = %v, want ErrEmptySegment", tt.in, err)
			}
		})
	}
}

func TestRender_SanitizesEverySegment(t *testing.T) {
	fields := completeFields()
	fields.Author = "Night/Owl"
	fields.Title = "a: b"

	got, err := mustParse(t, "{author}/{title}.{ext}").Render(fields)
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}

	want := filepath.Join("Night_Owl", "a_ b.mkv")
	if got.Path != want {
		t.Errorf("Render() = %q, want %q", got.Path, want)
	}
}

func TestRender_ValuesCannotEscapeTheLibraryRoot(t *testing.T) {
	// A broadcast title is attacker-controlled: anyone who can set a
	// stream title can set this string. Only the template's own literals
	// may produce a path component.
	tests := []struct {
		name  string
		title string
	}{
		{name: "posix traversal", title: "../../etc/passwd"},
		{name: "windows traversal", title: `..\..\windows\system32`},
		{name: "absolute posix path", title: "/etc/shadow"},
		{name: "absolute windows path", title: `C:\Windows\System32`},
		{name: "mixed separators", title: `a/b\c`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := completeFields()
			fields.Title = tt.title

			got, err := mustParse(t, "{author}/{title}.{ext}").Render(fields)
			if err != nil {
				t.Fatalf("Render() err = %v, want nil", err)
			}

			segments := strings.Split(got.Path, string(filepath.Separator))
			if len(segments) != 2 {
				t.Errorf("Render() = %q, split into %d segments, want exactly 2 from the template",
					got.Path, len(segments))
			}
			for _, segment := range segments {
				if segment == ".." || segment == "." {
					t.Errorf("Render() = %q contains a traversal segment", got.Path)
				}
			}
			if filepath.IsAbs(got.Path) {
				t.Errorf("Render() = %q is absolute, want a path relative to the library root", got.Path)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Truncation
// ///////////////////////////////////////////////

func TestRender_ShortensAnOverlongTitle(t *testing.T) {
	fields := completeFields()
	fields.Title = strings.Repeat("long title ", 60)

	got, err := mustParse(t, DefaultTemplate).Render(fields)
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}
	if !got.TitleShortened {
		t.Error("Render() TitleShortened = false, want true")
	}

	for segment := range strings.SplitSeq(got.Path, string(filepath.Separator)) {
		if n := len([]rune(segment)); n > maxSegmentUnits {
			t.Errorf("segment %q is %d runes, want at most %d", segment, n, maxSegmentUnits)
		}
	}
}

func TestRender_ShortenedTitleKeepsWholeRunes(t *testing.T) {
	fields := completeFields()
	fields.Title = strings.Repeat("配信タイトル", 60)

	got, err := mustParse(t, DefaultTemplate).Render(fields)
	if err != nil {
		t.Fatalf("Render() err = %v, want nil", err)
	}
	// A cut inside a multi-byte rune yields U+FFFD when decoded.
	if strings.ContainsRune(got.Path, '�') {
		t.Errorf("Render() = %q, want no replacement runes from a mid-rune cut", got.Path)
	}
}

func TestRender_CannotFitWithoutATitle(t *testing.T) {
	fields := completeFields()
	fields.Author = strings.Repeat("a", maxSegmentUnits+50)

	_, err := mustParse(t, "{author} - {title}.{ext}").Render(fields)
	if !errors.Is(err, ErrCannotFit) {
		t.Errorf("Render() err = %v, want ErrCannotFit", err)
	}
}

func TestShrink(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		overflow int
		want     string
	}{
		{name: "cuts back to a word boundary", title: "alpha beta gamma", overflow: 3, want: "alpha beta"},
		{name: "removes everything when overflow exceeds length", title: "alpha", overflow: 99, want: ""},
		{name: "trims a trailing separator", title: "alpha beta - ", overflow: 2, want: "alpha beta"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shrink(tt.title, tt.overflow); got != tt.want {
				t.Errorf("shrink(%q, %d) = %q, want %q", tt.title, tt.overflow, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Deduplicate
// ///////////////////////////////////////////////

func TestDeduplicate(t *testing.T) {
	tests := []struct {
		name  string
		taken map[string]bool
		want  string
	}{
		{
			name:  "free name is returned unchanged",
			taken: map[string]bool{},
			want:  filepath.Join("ExampleChannel", "stream.mkv"),
		},
		{
			name:  "first collision",
			taken: map[string]bool{filepath.Join("ExampleChannel", "stream.mkv"): true},
			want:  filepath.Join("ExampleChannel", "stream (2).mkv"),
		},
		{
			name: "second collision",
			taken: map[string]bool{
				filepath.Join("ExampleChannel", "stream.mkv"):     true,
				filepath.Join("ExampleChannel", "stream (2).mkv"): true,
			},
			want: filepath.Join("ExampleChannel", "stream (3).mkv"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Deduplicate(filepath.Join("ExampleChannel", "stream.mkv"), func(p string) bool {
				return tt.taken[p]
			})
			if err != nil {
				t.Fatalf("Deduplicate() err = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Deduplicate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeduplicate_Exhausted(t *testing.T) {
	_, err := Deduplicate("stream.mkv", func(string) bool { return true })
	if err == nil {
		t.Error("Deduplicate() err = nil, want an error once every candidate is taken")
	}
}

// TestSanitizeSegment_RefusesASegmentThatWalksTheTree covers the values a
// remote display name can take that would otherwise render a component
// naming a parent directory.
//
// Trailing dots and Unicode spaces are stripped by different rules, so a
// segment mixing them needs more than one pass before the result is stable.
// A component of ".." is what turns a rendered name into a path that leaves
// the library root, and the root is where the purge deletes.
func TestSanitizeSegment_RefusesASegmentThatWalksTheTree(t *testing.T) {
	cases := []struct {
		name    string
		segment string
	}{
		{"dots behind a no-break space", "..\u00A0."},
		{"dots behind an em space", "..\u2003."},
		{"dots behind an ideographic space", "..\u3000."},
		{"a bare parent", ".."},
		{"a bare current directory", "."},
		{"dots and spaces only", ". . ."},
		{"a parent with a trailing space", ".. "},
		{"a no-break space between every dot", "\u00A0.\u00A0.\u00A0"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizeSegment(tt.segment)
			if err == nil {
				t.Fatalf("SanitizeSegment(%q) = %q, want a refusal", tt.segment, got)
			}
			if !errors.Is(err, ErrEmptySegment) {
				t.Errorf("err = %v, want ErrEmptySegment so the caller parks rather than failing", err)
			}
		})
	}
}

func TestSanitizeSegment_RefusesEveryNameTheFilesystemLayersWillRefuse(t *testing.T) {
	// A component this accepts and a layer below refuses is contained
	// nowhere: the recording fails to move on every sweep, forever, with a
	// raw filesystem error and nothing that parks it with a reason. The
	// two containment checks downstream both ask filepath.IsLocal, so this
	// has to agree with it.
	tests := []struct {
		name    string
		segment string
	}{
		{name: "an ASCII device name", segment: "COM1"},
		{name: "a superscript device name", segment: "COM¹"},
		{name: "a superscript device name with an extension", segment: "COM¹.mkv"},
		{name: "a superscript LPT", segment: "LPT²"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizeSegment(tt.segment)
			if err != nil {
				return // refused outright, which is also containment
			}
			if !filepath.IsLocal(got) {
				t.Errorf("SanitizeSegment(%q) = %q, which filepath.IsLocal refuses", tt.segment, got)
			}
		})
	}
}

func TestRender_FitsAComponentTheFilesystemWillAccept(t *testing.T) {
	// An astral rune is one rune, two UTF-16 units and four UTF-8 bytes,
	// so counting runes admits a name neither NTFS nor ext4 will create.
	// The refusal then arrives from the open() a layer below this, where
	// nothing parks the recording with a reason and every sweep repeats it.
	template, err := Parse(DefaultTemplate)
	if err != nil {
		t.Fatalf("Parse() err = %v, want nil", err)
	}

	result, err := template.Render(Fields{
		Platform:  "twitch",
		Channel:   "examplechannel",
		Author:    "ExampleChannel",
		Title:     strings.Repeat("\U0001F600", 300),
		Category:  "Just Chatting",
		StartedAt: time.Date(2026, time.March, 4, 21, 15, 0, 0, time.UTC),
		Extension: "mkv",
	})
	if err != nil {
		// Refusing is the other acceptable answer: the caller parks the
		// recording and states the reason.
		return
	}

	for segment := range strings.SplitSeq(filepath.ToSlash(result.Path), "/") {
		if units := segmentUnits(segment); units > maxSegmentUnits {
			t.Errorf("segment %q measures %d units, want at most %d", segment, units, maxSegmentUnits)
		}
	}
}
