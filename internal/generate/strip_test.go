package generate

import (
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// StripLeadingBanner
// ///////////////////////////////////////////////

func TestStripLeadingBanner_RemovesContiguousCommentAndBlankBlock(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "banner then value",
			input: "# Auto-generated\n" +
				"#\n" +
				"# section title\n" +
				"\n" +
				"version = 1\n",
			want: "version = 1\n",
		},
		{
			name:  "no banner leaves content untouched",
			input: "version = 1\nname = \"x\"\n",
			want:  "version = 1\nname = \"x\"\n",
		},
		{
			name:  "all comments strips everything",
			input: "# one\n# two\n",
			want:  "",
		},
		{
			name:  "indented comment is still a comment",
			input: "  # indented\nvalue = 1\n",
			want:  "value = 1\n",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name: "banner stops at first real line, preserves later comments",
			input: "# banner\n" +
				"version = 1\n" +
				"# inline section header\n" +
				"[capture]\n",
			want: "version = 1\n" +
				"# inline section header\n" +
				"[capture]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(StripLeadingBanner([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("StripLeadingBanner(%q) =\n%q\nwant\n%q", tt.input, got, tt.want)
			}
		})
	}
}

// StripLeadingBanner must be idempotent: stripping a file that carries no
// banner returns the same bytes. A caller that strips twice must not lose a
// line of content to the second call.
func TestStripLeadingBanner_Idempotent(t *testing.T) {
	input := "# banner\nversion = 1\n"
	once := StripLeadingBanner([]byte(input))
	twice := StripLeadingBanner(once)
	if string(once) != string(twice) {
		t.Errorf("not idempotent: once=%q twice=%q", once, twice)
	}
	if !strings.HasPrefix(string(once), "version") {
		t.Errorf("banner not stripped on first call: %q", once)
	}
}
