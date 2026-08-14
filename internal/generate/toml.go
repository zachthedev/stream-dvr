package generate

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// FieldDoc documents one config field or section. [TOMLConfig.Docs] maps a
// dotted path such as "capture.poll_interval" to a FieldDoc, which is what
// [TOMLConfig.Generate] injects as comments.
type FieldDoc struct {
	// Comment is a multi-line description placed above the field.
	Comment string
	// Alternatives are extra commented-out example lines emitted below the
	// value (e.g., "# path = \"/alt/location\"").
	Alternatives []string
}

// TOMLConfig configures the generator that turns a struct into documented
// TOML.
type TOMLConfig struct {
	// ProjectName appears in the generated file's banner.
	ProjectName string
	// Defaults is the struct (or map) to serialize as TOML. Typically
	// the value returned by the project's DefaultConfig function.
	Defaults any
	// Docs maps dotted field paths to [FieldDoc] entries. A path the
	// encoder writes no key for is emitted as a commented-out block, so a
	// field left at its zero value is still documented.
	Docs map[string]FieldDoc
}

// ///////////////////////////////////////////////
// TOMLConfig methods
// ///////////////////////////////////////////////

// Generate returns the formatted TOML config bytes, and is suitable as the
// Generate function of an [OutputEntry]. When entry.Template is true, the
// header addresses the operator who edits the file and the contributor who
// regenerates it.
func (c TOMLConfig) Generate(entry OutputEntry) ([]byte, error) {
	var raw bytes.Buffer
	if err := toml.NewEncoder(&raw).Encode(c.Defaults); err != nil {
		return nil, fmt.Errorf("generate: marshaling TOML: %w", err)
	}

	lines := strings.Split(raw.String(), "\n")
	tables := tablePaths(lines)

	out := c.bannerLines(entry.Template)
	emitted := map[string]bool{}
	var sectionStack []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// An array-of-tables header opens a section the same way a plain one
		// does. Reading it as an ordinary line leaves the stack on the table
		// before it, so every key under it resolves against that table.
		if strings.HasPrefix(trimmed, "[") {
			c.injectOmitted(&out, sectionStack, emitted, tables)
			section := strings.Trim(trimmed, "[] ")
			sectionStack = strings.Split(section, ".")

			// An array-of-tables section repeats once per entry, and its
			// banner and comment describe the section rather than the entry.
			if !emitted[section] {
				emitted[section] = true

				out = append(out, "")
				out = append(out, fmt.Sprintf("# ///// %s /////", sectionLabel(section)))
				out = append(out, "")

				if doc, ok := c.Docs[section]; ok && doc.Comment != "" {
					for cl := range strings.SplitSeq(doc.Comment, "\n") {
						out = append(out, commentLine(cl))
					}
				}
			}
			out = append(out, trimmed)
			continue
		}

		if !strings.Contains(trimmed, "=") || strings.HasPrefix(trimmed, "#") {
			out = append(out, trimmed)
			continue
		}

		key := strings.TrimSpace(strings.SplitN(trimmed, "=", 2)[0])
		fullPath := key
		if len(sectionStack) > 0 {
			fullPath = strings.Join(sectionStack, ".") + "." + key
		}
		documented := emitted[fullPath]
		emitted[fullPath] = true

		doc, ok := c.Docs[fullPath]
		if !ok || documented {
			out = append(out, trimmed)
			continue
		}
		if doc.Comment != "" {
			for cl := range strings.SplitSeq(doc.Comment, "\n") {
				out = append(out, commentLine(cl))
			}
		}
		out = append(out, trimmed)
		for _, alt := range doc.Alternatives {
			out = append(out, commentLine(alt))
		}
	}

	c.injectOmitted(&out, sectionStack, emitted, tables)
	c.injectTopLevel(&out, emitted)

	result := strings.Join(out, "\n")
	result = strings.TrimRight(result, "\n") + "\n"
	return []byte(result), nil
}

// ///////////////////////////////////////////////
// Internal helpers
// ///////////////////////////////////////////////

// bannerLines returns the file-header comment block.
//
// A template is committed and also copied to an operator's disk, so its
// header speaks to both audiences: an operator told to edit the file must
// not open it on an instruction not to. A caller that wants no header at
// all can drop it with [StripLeadingBanner].
func (c TOMLConfig) bannerLines(template bool) []string {
	var head []string
	if template {
		head = []string{
			fmt.Sprintf("# %s config. Defaults generated; edit this copy freely.", c.ProjectName),
			"# Contributors: update internal/config/*.go and run `make generate`.",
		}
	} else {
		head = []string{"# " + GeneratedByHeader}
	}
	return append(head,
		"#",
		"# ///////////////////////////////////////////////",
		fmt.Sprintf("# %s Configuration", c.ProjectName),
		"# ///////////////////////////////////////////////",
		"",
	)
}

// injectOmitted appends commented-out entries for Docs keys belonging to the
// current section that the encoder wrote no line for, which is what an
// omitempty field at its zero value produces. Keys are sorted, so the output
// is the same on every run.
//
// tables names every path the encoder wrote a header for. A subtable's own
// doc entry sits directly under its parent's prefix and has the shape of an
// omitted key, so without tables it lands in the parent as an orphan comment
// block and is emitted again above its real header.
func (c TOMLConfig) injectOmitted(out *[]string, sectionStack []string, emitted, tables map[string]bool) {
	if len(sectionStack) == 0 {
		return
	}
	prefix := strings.Join(sectionStack, ".") + "."

	var omitted []string
	for path := range c.Docs {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if strings.Contains(rest, ".") {
			continue
		}
		if emitted[path] || tables[path] {
			continue
		}
		omitted = append(omitted, path)
	}
	slices.Sort(omitted)

	for _, path := range omitted {
		doc := c.Docs[path]
		*out = append(*out, "")
		if doc.Comment != "" {
			for cl := range strings.SplitSeq(doc.Comment, "\n") {
				*out = append(*out, commentLine(cl))
			}
		}
		for _, alt := range doc.Alternatives {
			*out = append(*out, commentLine(alt))
		}
		emitted[path] = true
	}
}

// injectTopLevel appends docs for top-level keys the encoder writes no line
// for at all, such as an empty map or slice.
func (c TOMLConfig) injectTopLevel(out *[]string, emitted map[string]bool) {
	var keys []string
	for path := range c.Docs {
		if strings.Contains(path, ".") || emitted[path] {
			continue
		}
		keys = append(keys, path)
	}
	slices.Sort(keys)

	for _, key := range keys {
		doc := c.Docs[key]
		label := sectionLabel(key)
		*out = append(*out, "")
		*out = append(*out, fmt.Sprintf("# ///// %s /////", label))
		*out = append(*out, "")
		if doc.Comment != "" {
			for cl := range strings.SplitSeq(doc.Comment, "\n") {
				*out = append(*out, commentLine(cl))
			}
		}
		for _, alt := range doc.Alternatives {
			*out = append(*out, commentLine(alt))
		}
		emitted[key] = true
	}
}

// tablePaths returns the dotted path of every table header in encoded TOML,
// counting the array-of-tables form, which opens a section the same way.
func tablePaths(lines []string) map[string]bool {
	paths := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		paths[strings.Trim(trimmed, "[] ")] = true
	}
	return paths
}

// commentLine formats one comment line. Empty input produces a bare "#", so
// a blank comment line carries no trailing whitespace.
func commentLine(text string) string {
	if text == "" {
		return "#"
	}
	return "# " + text
}

// sectionLabel returns a readable label for a section: its last dotted
// segment, with the first letter capitalized.
func sectionLabel(section string) string {
	parts := strings.Split(section, ".")
	last := parts[len(parts)-1]
	if last == "" {
		return ""
	}
	return strings.ToUpper(last[:1]) + last[1:]
}
