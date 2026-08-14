package generate

import (
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// exampleEnv is a schema with one of everything the renderer reads.
func exampleEnv() DotEnv {
	return DotEnv{
		ProjectName: "example",
		Consumer:    "The Makefile",
		Variables: []EnvVar{
			{
				Name:    "EXAMPLE_TOKEN",
				Purpose: "An application id the build stamps into the binary.",
				Example: "abcdef0123456789",
				Absent:  "the feature it enables is unavailable",
			},
			{Name: "EXAMPLE_PLAIN", Purpose: "A setting with no example."},
		},
	}
}

// renderEnv generates a template or fails the test.
func renderEnv(t *testing.T, schema DotEnv, entry OutputEntry) string {
	t.Helper()

	body, err := schema.Generate(entry)
	if err != nil {
		t.Fatalf("Generate() err = %v, want nil", err)
	}
	return string(body)
}

// ///////////////////////////////////////////////
// What the template must contain
// ///////////////////////////////////////////////

func TestGenerate_DocumentsEveryDeclaredVariable(t *testing.T) {
	// A schema entry the template omits is a setting nobody can discover,
	// which is the drift generating the file is meant to end.
	rendered := renderEnv(t, exampleEnv(), OutputEntry{Path: ".env.template", Template: true})

	for _, variable := range exampleEnv().Variables {
		if !strings.Contains(rendered, variable.Name) {
			t.Errorf("the template does not mention %s", variable.Name)
		}
	}
	if !strings.Contains(rendered, "the feature it enables is unavailable") {
		t.Error("the template does not say what happens without a variable")
	}
}

func TestGenerate_CommentsOutEveryAssignment(t *testing.T) {
	// The safety property. A template copied whole must change nothing
	// until a line is uncommented: make treats an empty assignment as a
	// value, so a live VERSION= stamps a binary claiming no version and a
	// live REPO_OWNER= points its links at an account that does not exist,
	// each overriding what the build would otherwise read from git.
	rendered := renderEnv(t, exampleEnv(), OutputEntry{Path: ".env.template", Template: true})

	for line := range strings.SplitSeq(rendered, "\n") {
		if strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			t.Errorf("the template carries a live assignment: %q", line)
		}
	}
}

func TestGenerate_SwapsTheBannerForATemplate(t *testing.T) {
	// A file an operator is told to copy and edit must not open by telling
	// them not to edit it.
	template := renderEnv(t, exampleEnv(), OutputEntry{Path: ".env.template", Template: true})
	artifact := renderEnv(t, exampleEnv(), OutputEntry{Path: ".env.generated"})

	if strings.Contains(template, GeneratedByHeader) {
		t.Error("the template carries the do-not-edit banner")
	}
	if !strings.Contains(artifact, GeneratedByHeader) {
		t.Error("a non-template output is missing the do-not-edit banner")
	}
}

func TestGenerate_EndsWithANewline(t *testing.T) {
	// A file without one reads as modified to every tool that appends.
	rendered := renderEnv(t, exampleEnv(), OutputEntry{Path: ".env.template", Template: true})

	if !strings.HasSuffix(rendered, "\n") {
		t.Error("the template does not end with a newline")
	}
}

// ///////////////////////////////////////////////
// What the renderer refuses
// ///////////////////////////////////////////////

func TestGenerate_RefusesASchemaThatCannotProduceATemplate(t *testing.T) {
	cases := []struct {
		name   string
		schema DotEnv
	}{
		{
			name:   "no variables at all",
			schema: DotEnv{ProjectName: "example"},
		},
		{
			name: "a variable with no name",
			schema: DotEnv{ProjectName: "example", Variables: []EnvVar{
				{Purpose: "a setting nobody can set"},
			}},
		},
		{
			name: "one name declared twice",
			schema: DotEnv{ProjectName: "example", Variables: []EnvVar{
				{Name: "EXAMPLE_TOKEN", Purpose: "first"},
				{Name: "EXAMPLE_TOKEN", Purpose: "second"},
			}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.schema.Generate(OutputEntry{Path: ".env.template"}); err == nil {
				t.Error("Generate() err = nil, want a refusal")
			}
		})
	}
}

// ///////////////////////////////////////////////
// Wrapping
// ///////////////////////////////////////////////

func TestAppendWrapped_NeverSplitsAWord(t *testing.T) {
	// A purpose carries the URL an operator registers an application at. A
	// wrap through the middle of one leaves an address that cannot be
	// clicked or copied.
	const url = "https://dev.twitch.tv/console/apps"
	prose := "Register a Public client at " + url + " with the OAuth redirect http://localhost."

	var found bool
	for _, line := range appendWrapped(nil, prose) {
		if len(line) > commentWidth {
			t.Errorf("line runs to %d characters, want at most %d: %q", len(line), commentWidth, line)
		}
		if strings.Contains(line, url) {
			found = true
		}
	}
	if !found {
		t.Errorf("%q was split across lines", url)
	}
}

func TestAppendWrapped_KeepsEveryWord(t *testing.T) {
	// Wrapping that dropped a word would lose part of an explanation with
	// nothing to show that it had.
	prose := strings.Repeat("word ", 60)

	joined := strings.Join(appendWrapped(nil, prose), " ")
	if got := strings.Count(joined, "word"); got != 60 {
		t.Errorf("kept %d words, want 60", got)
	}
}
