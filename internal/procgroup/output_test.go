package procgroup

import (
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Output
// ///////////////////////////////////////////////

func TestNewTailOutput_KeepsTheClosingBytesAndCountsTheRest(t *testing.T) {
	// For a tool that states its answer after the work. A leading capture
	// discards exactly the part worth reading when a decoder warns once per
	// frame and the measurement is printed at the end.
	tests := []struct {
		name        string
		limit       int
		writes      []string
		want        string
		wantDropped int
	}{
		{
			name:   "everything fits",
			limit:  16,
			writes: []string{"ffmpeg ", "version"},
			want:   "ffmpeg version",
		},
		{
			name:        "one write overruns the limit",
			limit:       4,
			writes:      []string{"abcdefgh"},
			want:        "efgh",
			wantDropped: 4,
		},
		{
			name:        "the limit is reached partway through",
			limit:       6,
			writes:      []string{"abcd", "efgh"},
			want:        "cdefgh",
			wantDropped: 2,
		},
		{
			// The case the whole thing exists for: the answer arrives last,
			// after far more noise than the bound holds.
			name:        "the answer follows a flood",
			limit:       8,
			writes:      []string{"noisenoisenoisenoise", "ANSWER!!"},
			want:        "ANSWER!!",
			wantDropped: 20,
		},
		{
			name:   "no writes at all",
			limit:  8,
			writes: nil,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := NewTailOutput(tt.limit)
			for _, write := range tt.writes {
				n, err := out.Write([]byte(write))
				if err != nil {
					t.Fatalf("Write() err = %v, want nil", err)
				}
				// A short write stalls the tool on its own output, which
				// stops it doing the work it was started for.
				if n != len(write) {
					t.Errorf("Write() = %d, want the whole %d bytes accepted", n, len(write))
				}
			}

			if got := out.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if got := out.Dropped(); got != tt.wantDropped {
				t.Errorf("Dropped() = %d, want %d", got, tt.wantDropped)
			}
		})
	}
}

func TestExcerpt_OnATailCaptureQuotesTheEnd(t *testing.T) {
	// The one path where an operator needs the tool's own words. A tail
	// capture keeps the closing bytes precisely because the reason a run
	// failed is stated there, and cutting the opening of what survived
	// reports the middle of the stream instead.
	out := NewTailOutput(4 << 10)
	for range 500 {
		_, _ = out.Write([]byte("[in#0] STSC entry is invalid, skipping\n"))
	}
	_, _ = out.Write([]byte("Conversion failed! THE ACTUAL REASON\n"))

	excerpt := out.Excerpt(256)
	if !strings.Contains(excerpt, "THE ACTUAL REASON") {
		t.Errorf("excerpt = %q, want the reason the tool gave", excerpt)
	}
	if len(excerpt) > 256+64 {
		t.Errorf("excerpt is %d bytes, want it cut near the limit", len(excerpt))
	}
}

func TestNewOutput_KeepsTheLeadingBytesAndCountsTheRest(t *testing.T) {
	tests := []struct {
		name        string
		limit       int
		writes      []string
		want        string
		wantDropped int
	}{
		{
			name:   "everything fits",
			limit:  16,
			writes: []string{"ffmpeg ", "version"},
			want:   "ffmpeg version",
		},
		{
			name:        "one write overruns the limit",
			limit:       4,
			writes:      []string{"abcdefgh"},
			want:        "abcd",
			wantDropped: 4,
		},
		{
			name:        "the limit is reached partway through",
			limit:       6,
			writes:      []string{"abcd", "efgh"},
			want:        "abcdef",
			wantDropped: 2,
		},
		{
			name:        "writes continue after the limit is full",
			limit:       2,
			writes:      []string{"ab", "cd", "ef"},
			want:        "ab",
			wantDropped: 4,
		},
		{
			// A tool that writes nothing is the ordinary quiet success.
			name:   "no writes at all",
			limit:  8,
			writes: nil,
			want:   "",
		},
		{
			// Nothing may be kept, and the tool must still not block.
			name:        "a limit of zero keeps nothing",
			limit:       0,
			writes:      []string{"abc"},
			want:        "",
			wantDropped: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := NewOutput(tt.limit)
			for _, write := range tt.writes {
				n, err := out.Write([]byte(write))
				if err != nil {
					t.Fatalf("Write(%q) err = %v, want nil", write, err)
				}
				// A short write stops the tool doing the work it was
				// started for, so every byte is always reported taken.
				if n != len(write) {
					t.Errorf("Write(%q) = %d, want %d", write, n, len(write))
				}
			}

			if got := out.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if got := string(out.Bytes()); got != tt.want {
				t.Errorf("Bytes() = %q, want %q", got, tt.want)
			}
			if got := out.Dropped(); got != tt.wantDropped {
				t.Errorf("Dropped() = %d, want %d", got, tt.wantDropped)
			}
			if got, want := out.Truncated(), tt.wantDropped > 0; got != want {
				t.Errorf("Truncated() = %t, want %t", got, want)
			}
		})
	}
}

func TestExcerpt_SaysHowMuchWasLeftOut(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		excerpt int
		write   string
		want    string
	}{
		{
			name:    "short text passes through",
			limit:   64,
			excerpt: 32,
			write:   "  No playable streams found  ",
			want:    "No playable streams found",
		},
		{
			name:    "text longer than the excerpt is cut and counted",
			limit:   64,
			excerpt: 4,
			write:   "abcdefghij",
			want:    "abcd (6 more bytes)",
		},
		{
			// What the writer discarded counts toward the total, or an
			// error reports far less missing than really was.
			name:    "bytes dropped at the writer are counted too",
			limit:   6,
			excerpt: 4,
			write:   "abcdefghij",
			want:    "abcd (6 more bytes)",
		},
		{
			name:    "empty output reads as empty",
			limit:   16,
			excerpt: 8,
			write:   "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := NewOutput(tt.limit)
			if _, err := out.Write([]byte(tt.write)); err != nil {
				t.Fatalf("Write() err = %v, want nil", err)
			}

			if got := out.Excerpt(tt.excerpt); got != tt.want {
				t.Errorf("Excerpt(%d) = %q, want %q", tt.excerpt, got, tt.want)
			}
		})
	}
}

func TestExcerpt_CountsWhatTheCallerAlreadyLost(t *testing.T) {
	// Callers that never held an Output reach the same wording through here,
	// so a tool quoted from a parsed field and one quoted from a pipe read
	// alike in a log line and a notification body.
	tests := []struct {
		name    string
		text    string
		omitted int
		limit   int
		want    string
	}{
		{
			name:  "text within the limit keeps its own words and gains nothing",
			text:  "  No playable streams found  ",
			limit: 64,
			want:  "No playable streams found",
		},
		{
			name:  "text past the limit is cut and the loss counted",
			text:  "abcdefghij",
			limit: 4,
			want:  "abcd (6 more bytes)",
		},
		{
			// The caller's own loss is added to the cut, or an error reports
			// far less missing than really was.
			name:    "a loss the caller already knows about is added to the cut",
			text:    "abcdefghij",
			omitted: 90,
			limit:   4,
			want:    "abcd (96 more bytes)",
		},
		{
			name:    "a loss with nothing cut is still reported",
			text:    "abc",
			omitted: 7,
			limit:   64,
			want:    "abc (7 more bytes)",
		},
		{
			name:  "empty text reads as empty",
			limit: 64,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Excerpt(tt.text, tt.omitted, tt.limit); got != tt.want {
				t.Errorf("Excerpt(%q, %d, %d) = %q, want %q", tt.text, tt.omitted, tt.limit, got, tt.want)
			}
		})
	}
}

func TestExcerpt_NeverCutsARuneInHalf(t *testing.T) {
	// The text is the tool's own words, and a cut mid-rune replaces the
	// last of them with a replacement character.
	out := NewOutput(64)
	if _, err := out.Write([]byte(strings.Repeat("é", 8))); err != nil {
		t.Fatalf("Write() err = %v, want nil", err)
	}

	// Each rune is two bytes, so an odd limit lands inside one.
	got := out.Excerpt(5)
	if !strings.HasPrefix(got, "éé") {
		t.Fatalf("Excerpt() = %q, want it to start with whole runes", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("Excerpt() = %q, want no replacement character", got)
	}
}
