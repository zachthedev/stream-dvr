package generate

import (
	"fmt"
	"strings"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// EnvVar documents one environment variable for [DotEnv].
//
// It is this package's own shape rather than the declaring package's, for
// the reason FieldDoc is: the package that owns the schema must not have to
// import a generator to describe itself. Whoever registers the entry copies
// between the two.
type EnvVar struct {
	// Name is the variable, spelled as the consumer reads it.
	Name string
	// Purpose says what the value is for.
	Purpose string
	// Example is a value of the right shape, shown in a comment.
	Example string
	// Absent says what happens without the variable.
	Absent string
}

// DotEnv configures the environment-template generator.
type DotEnv struct {
	// ProjectName appears in the generated file's banner.
	ProjectName string
	// Consumer names what reads the file, so the banner can say how a
	// value here reaches a build.
	Consumer string
	// Variables are the settings to document, in the order given.
	Variables []EnvVar
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// commentWidth is where a purpose is wrapped.
//
// Short enough that the file reads in a narrow terminal, which is where a
// template is most often opened.
const commentWidth = 74

// ///////////////////////////////////////////////
// DotEnv methods
// ///////////////////////////////////////////////

// Generate returns the environment template. Suitable as the Generate
// function of an [OutputEntry].
//
// Every assignment is emitted commented out. A template copied whole must
// change nothing until a line is uncommented, because a consumer that treats
// an empty value as set would otherwise be handed one for every variable at
// once, and an empty version stamps a binary that claims no version at all.
func (d DotEnv) Generate(entry OutputEntry) ([]byte, error) {
	if len(d.Variables) == 0 {
		return nil, fmt.Errorf("generate: %s declares no variables", entry.Path)
	}

	out := d.bannerLines(entry.Template)
	seen := map[string]bool{}

	for _, variable := range d.Variables {
		if variable.Name == "" {
			return nil, fmt.Errorf("generate: %s has a variable with no name", entry.Path)
		}
		if seen[variable.Name] {
			return nil, fmt.Errorf("generate: %s declares %s twice", entry.Path, variable.Name)
		}
		seen[variable.Name] = true

		out = append(out, "", fmt.Sprintf("# ///// %s /////", variable.Name), "")
		out = appendWrapped(out, variable.Purpose)
		if variable.Absent != "" {
			out = append(out, "#")
			out = appendWrapped(out, "Unset: "+variable.Absent+".")
		}
		if variable.Example != "" {
			out = append(out, fmt.Sprintf("#%s=%s", variable.Name, variable.Example))
			continue
		}
		out = append(out, "#"+variable.Name+"=")
	}

	return []byte(strings.Join(out, "\n") + "\n"), nil
}

// bannerLines is the header every generated template opens with.
func (d DotEnv) bannerLines(template bool) []string {
	head := []string{"# " + GeneratedByHeader}
	if template {
		head = []string{
			fmt.Sprintf("# %s build environment. Copy to .env and uncomment what you need.", d.ProjectName),
			"# Contributors: update internal/buildenv/*.go and run `make generate`.",
		}
	}

	out := append(head,
		"#",
		"# ///////////////////////////////////////////////",
		fmt.Sprintf("# %s Build Environment", d.ProjectName),
		"# ///////////////////////////////////////////////",
		"#",
	)
	if d.Consumer != "" {
		out = appendWrapped(out, fmt.Sprintf(
			"%s reads .env when it exists, so a value set there reaches every build "+
				"without being typed. .env is not committed and this template is, "+
				"which is why every line below is commented out and holds no value.",
			d.Consumer))
	}
	return out
}

// ///////////////////////////////////////////////
// Internal helpers
// ///////////////////////////////////////////////

// appendWrapped adds prose as comment lines, wrapped at commentWidth.
//
// Wrapping is by word and never mid-word, so a URL in a purpose stays
// clickable rather than being split across two lines.
func appendWrapped(out []string, prose string) []string {
	line := "#"
	for word := range strings.FieldsSeq(prose) {
		if len(line)+1+len(word) > commentWidth && line != "#" {
			out = append(out, line)
			line = "#"
		}
		line += " " + word
	}
	if line != "#" {
		out = append(out, line)
	}
	return out
}
