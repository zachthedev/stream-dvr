package escape

import (
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// ///////////////////////////////////////////////
// Text
// ///////////////////////////////////////////////

func TestText_LeavesPrintableTextAlone(t *testing.T) {
	// Quoting everything would make ordinary output unreadable, which is
	// how a helper like this ends up bypassed.
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "a plain message", text: "recording complete"},
		{name: "spaces and punctuation", text: "ExampleChannel - 2026-08-12 - Building a DVR"},
		{name: "a windows path", text: `D:\library\ExampleChannel\2026\one.mkv`},
		{name: "quotes are printable", text: `he said "hello"`},
		{name: "emoji", text: "stream starting 🎬"},
		{name: "non-latin script", text: "配信開始"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.text); got != tt.text {
				t.Errorf("Text(%q) = %q, want it returned unchanged", tt.text, got)
			}
		})
	}
}

func TestText_QuotesAnythingUnprintable(t *testing.T) {
	// Each of these reaches a log file or a terminal from a stream title,
	// a category, or a filename, none of which this project writes.
	tests := []struct {
		name string
		text string
		why  string
	}{
		{
			name: "a carriage return and newline",
			text: "title\r\n2026-08-12T00:00:00.000Z [INFO] recording complete",
			why:  "forges a log record with a real timestamp and level",
		},
		{name: "a bare newline", text: "first\nsecond", why: "splits one record into two"},
		{name: "an escape sequence", text: "title\x1b[2J", why: "clears the reader's terminal"},
		{name: "a bell", text: "title\a", why: "reaches the terminal as a sound"},
		{name: "a carriage return alone", text: "title\roverwritten", why: "rewrites the line in place"},
		{name: "a tab", text: "a\tb", why: "is ambiguous against the field separator"},
		{name: "a null byte", text: "title\x00", why: "truncates the line for some readers"},
		{name: "delete", text: "title\x7f", why: "is a control character"},
		{name: "a zero width space", text: "ex\u200bamplechannel", why: "hides a difference between two names"},
		{name: "a bidirectional override", text: "title\u202e", why: "reorders the line without changing a byte"},
		{name: "invalid utf-8", text: "title\xff\xfe", why: "is not text at all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.text)
			if got == tt.text {
				t.Fatalf("Text(%q) returned it unchanged, but it %s", tt.text, tt.why)
			}

			// The whole point is that nothing unprintable survives, so
			// check the result rather than trusting that it changed.
			for _, r := range got {
				if !unicode.IsPrint(r) {
					t.Errorf("Text(%q) = %q, which still carries %U", tt.text, got, r)
				}
			}
		})
	}
}

func TestText_QuotedOutputStaysOnOneLine(t *testing.T) {
	// A log record is one line. Any escaping that emitted a real newline
	// would defeat itself.
	for _, text := range []string{"a\nb", "a\r\nb", "a\rb", "\n\n\n"} {
		if got := Text(text); strings.ContainsAny(got, "\r\n") {
			t.Errorf("Text(%q) = %q, want no line break in the result", text, got)
		}
	}
}

func TestText_IsRecoverable(t *testing.T) {
	// Quoting is chosen over stripping so the original is still readable.
	// A reader who unquotes the field has to get the exact bytes back.
	original := "title\r\nwith\x1bcontrol chars"

	unquoted, err := strconv.Unquote(Text(original))
	if err != nil {
		t.Fatalf("Unquote() err = %v, want the escaped form to be a valid Go literal", err)
	}
	if unquoted != original {
		t.Errorf("round trip = %q, want %q", unquoted, original)
	}
}

// ///////////////////////////////////////////////
// Distinguishability
// ///////////////////////////////////////////////

func TestText_TellsARealControlCharacterFromALiteralOne(t *testing.T) {
	// The package exists to preserve exactly this distinction. A title that
	// arrived carrying CR LF and a title made of printable characters that
	// read like the escaped form are different facts about who sent what,
	// and an operator has only the rendering to go on.
	carried := "a\r\nb"
	spelled := `"a\r\nb"`

	if Text(carried) == Text(spelled) {
		t.Errorf("Text(%q) and Text(%q) both render as %q", carried, spelled, Text(carried))
	}
}

func TestText_IsInjective(t *testing.T) {
	// Any two distinct inputs short enough to be reproduced whole must render
	// differently, or the rendering is not evidence of anything.
	inputs := []string{
		"",
		"plain",
		`"plain"`,
		`"a\r\nb"`,
		`a\r\nb`,
		"a\r\nb",
		`\`,
		`"`,
		`""`,
		"a\tb",
		`a\tb`,
		"title\x1b[2J",
		`title\x1b[2J`,
		"配信開始",
		"\xff",
	}

	seen := make(map[string]string, len(inputs))
	for _, input := range inputs {
		got := Text(input)
		if before, clash := seen[got]; clash {
			t.Errorf("Text(%q) and Text(%q) both = %q", before, input, got)
			continue
		}
		seen[got] = input
	}
}

func TestText_PlainOutputIsAlwaysTheInput(t *testing.T) {
	// A reader decides which shape they are holding by the first byte, so a
	// rendering that does not open with a quote has to be the arriving text.
	for _, input := range []string{"plain", `a\r\nb`, `it's fine`, "配信開始", ""} {
		got := Text(input)
		if strings.HasPrefix(got, `"`) {
			continue
		}
		if got != input {
			t.Errorf("Text(%q) = %q, want the unquoted shape to be the input itself", input, got)
		}
	}
}

func TestText_QuotesTextThatWouldReadAsAQuotedLiteral(t *testing.T) {
	// Printable text spelling out a Go literal is the one way the two shapes
	// could collide.
	got := Text(`"a\r\nb"`)

	unquoted, err := strconv.Unquote(got)
	if err != nil {
		t.Fatalf("Unquote(%q) err = %v, want a valid Go literal", got, err)
	}
	if unquoted != `"a\r\nb"` {
		t.Errorf("round trip = %q, want the input back", unquoted)
	}
}

// ///////////////////////////////////////////////
// Length
// ///////////////////////////////////////////////

func TestText_BoundsWhatItReproduces(t *testing.T) {
	// A subprocess's combined output reaches a log attribute with no bound of
	// its own, and a rotating log keeps a fixed number of bytes: one record
	// that long evicts every other record in the window.
	tests := []struct {
		name  string
		input string
	}{
		{name: "printable", input: strings.Repeat("a", 200_000)},
		{name: "unprintable", input: strings.Repeat("\x1b", 200_000)},
		{name: "multi-byte runes", input: strings.Repeat("配", 200_000)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.input)

			if len(got) > MaxOut {
				t.Errorf("Text() = %d bytes for a %d byte input, want at most MaxOut (%d)",
					len(got), len(tt.input), MaxOut)
			}
			if !strings.Contains(got, strconv.Itoa(len(tt.input))) {
				t.Errorf("Text() = %q, want the marker to state the %d bytes that arrived",
					lastRunes(got, 40), len(tt.input))
			}
		})
	}
}

func TestText_TruncationLeavesValidUTF8(t *testing.T) {
	// Cutting mid-rune would manufacture invalid UTF-8 out of text that
	// arrived valid, which is a claim about the sender that is not true.
	for _, offset := range []int{0, 1, 2} {
		input := strings.Repeat("x", offset) + strings.Repeat("配", 200_000)

		if got := Text(input); !utf8.ValidString(got) {
			t.Errorf("Text() with a %d byte lead-in produced invalid UTF-8", offset)
		}
	}
}

func TestText_LeavesTextAtTheLimitAlone(t *testing.T) {
	input := strings.Repeat("a", MaxLen)

	if got := Text(input); got != input {
		t.Errorf("Text() truncated a %d byte input, want MaxLen (%d) reproduced whole", len(input), MaxLen)
	}
}

// lastRunes returns the tail of s, for an assertion message about a very
// long rendering.
func lastRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

// ///////////////////////////////////////////////
// Field
// ///////////////////////////////////////////////

func TestField_QuotesAnythingCarryingARecordSeparator(t *testing.T) {
	// A log record is key=value pairs joined by commas, after a bar. Text
	// carrying any of those renders as fields of its own choosing unless it
	// is quoted.
	tests := []struct {
		name string
		text string
		why  string
	}{
		{
			name: "a comma and an equals",
			text: "movie night, level=FAIL, error=disk gone",
			why:  "forges two more attributes on the record",
		},
		{name: "an equals alone", text: "a=1", why: "splits one field into two"},
		{name: "a comma alone", text: "a, b", why: "opens a second field"},
		{name: "a bar", text: "night | user=root", why: "forges the whole attribute list"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Field(tt.text)
			if got == tt.text {
				t.Fatalf("Field(%q) returned it unchanged, but it %s", tt.text, tt.why)
			}
			if !strings.HasPrefix(got, `"`) {
				t.Errorf("Field(%q) = %q, want a quoted literal", tt.text, got)
			}

			unquoted, err := strconv.Unquote(got)
			if err != nil {
				t.Fatalf("Unquote(%q) err = %v", got, err)
			}
			if unquoted != tt.text {
				t.Errorf("round trip = %q, want %q", unquoted, tt.text)
			}
		})
	}
}

func TestField_LeavesOrdinaryFieldsAlone(t *testing.T) {
	// Quoting every field would make a log unreadable, which is how a helper
	// like this ends up bypassed.
	for _, text := range []string{"title", "recording complete", "ExampleChannel", `D:\recordings\one.mkv`} {
		if got := Field(text); got != text {
			t.Errorf("Field(%q) = %q, want it returned unchanged", text, got)
		}
	}
}

func TestField_QuotesEverythingTextDoes(t *testing.T) {
	// Field is Text plus one rule, so anything Text refuses to pass through
	// must not pass through here either.
	for _, text := range []string{"a\r\nb", "title\x1b[2J", `"quoted"`, "title\xff"} {
		if got := Field(text); got == text {
			t.Errorf("Field(%q) returned it unchanged, but Text() = %q", text, Text(text))
		}
	}
}

func TestField_IsBounded(t *testing.T) {
	input := strings.Repeat("a=b,", 100_000)

	if got := Field(input); len(got) > MaxOut {
		t.Errorf("Field() = %d bytes for a %d byte input, want at most MaxOut (%d)",
			len(got), len(input), MaxOut)
	}
}

func TestField_StaysOnOneLine(t *testing.T) {
	for _, text := range []string{"a\nb", "a\r\nb", strings.Repeat("a\n", 100_000)} {
		if got := Field(text); strings.ContainsAny(got, "\r\n") {
			t.Errorf("Field(%q) = %q, want no line break in the result", lastRunes(text, 20), lastRunes(got, 40))
		}
	}
}
