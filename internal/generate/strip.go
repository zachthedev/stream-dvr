package generate

import "bytes"

// StripLeadingBanner removes the run of blank and "#" comment lines at the
// start of content and returns the rest, which is a generated file with its
// header taken off.
//
// It recognizes only the comment syntaxes that mark a line with "#", which
// covers TOML, shell, and YAML. Any other syntax must strip its own way.
func StripLeadingBanner(content []byte) []byte {
	lines := bytes.SplitAfter(content, []byte{'\n'})
	i := 0
	for ; i < len(lines); i++ {
		trimmed := bytes.TrimLeft(lines[i], " \t")
		if len(trimmed) == 0 {
			continue
		}
		// bytes.SplitAfter keeps the trailing '\n', so a line holding
		// only "\n" counts as a blank separator.
		if len(trimmed) == 1 && trimmed[0] == '\n' {
			continue
		}
		if trimmed[0] == '#' {
			continue
		}
		break
	}
	return bytes.Join(lines[i:], nil)
}
