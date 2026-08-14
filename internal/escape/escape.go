// Package escape renders untrusted text safely for a log file or a terminal.
//
// Stream titles, categories, filenames, and remote metadata all reach
// human-readable output, and every one of them is written by someone else.
// Text carrying a line ending forges a second log record that looks exactly
// like a real one. Text carrying an escape sequence reaches the terminal of
// whoever reads the file, where it can clear the screen or rewrite the lines
// above it.
//
// # Reading a rendering
//
// Every rendering is one line and has one of three shapes, which a reader
// tells apart by the first byte:
//
//	plain text          the arriving text, byte for byte
//	"quoted literal"    a Go string literal, which strconv.Unquote turns
//	                    back into the exact bytes that arrived
//	"head" (truncated from N bytes)
//	                    the first MaxLen bytes, quoted, and the length that
//	                    arrived
//
// The shapes never collide, because text that itself opens with a double
// quote is rendered as a literal. That is what lets a reader tell a title
// carrying a real line feed from one spelling out a backslash and an n.
package escape

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ///////////////////////////////////////////////
// Limits
// ///////////////////////////////////////////////

// MaxLen bounds how many bytes of the arriving text a rendering reproduces.
//
// A rotating log keeps a fixed number of bytes, so one record carrying a
// subprocess's whole output evicts every other record in the retained
// window. A subprocess's combined output and a remote error message both
// reach a log attribute, and neither carries a bound of its own.
//
// The bound is on the input. Quoting expands a control byte to the four
// characters of `\x1b`, so a rendering runs to MaxOut.
const MaxLen = 4096

// MaxOut bounds a rendering, covering the widest quoting expansion and the
// truncation marker.
const MaxOut = 4*MaxLen + 64

// separators are the characters the log record format gives meaning to: the
// comma between attributes, the equals between a key and its value, and the
// bar between the message and the attribute list.
const separators = ",=|"

// ///////////////////////////////////////////////
// Rendering
// ///////////////////////////////////////////////

// Text renders s for a terminal or a display line.
//
// Printable text is returned unchanged, which keeps ordinary output readable
// and is what stops anyone routing around this to get a clean line. Anything
// else comes back quoted: a title that arrived carrying control characters is
// itself worth seeing, and removing them silently would hide that anything
// happened.
func Text(s string) string {
	return render(s, mustQuote(s))
}

// Field renders s for a key or a value inside a log record.
//
// It is Text plus one rule: text carrying a record separator is quoted, so
// every separator a reader sees belongs to the record rather than to the text
// inside it. A value reading `night, level=FAIL, error=gone` would otherwise
// render as three fields of its own choosing.
func Field(s string) string {
	return render(s, mustQuote(s) || strings.ContainsAny(s, separators))
}

// render produces one of the three documented shapes.
//
// Truncated output is always quoted, because the marker after the closing
// quote is what tells a reader where the reproduced bytes stopped. It is also
// the one lossy shape, which is why it announces itself.
func render(s string, quote bool) string {
	if len(s) > MaxLen {
		return fmt.Sprintf("%s (truncated from %d bytes)", strconv.Quote(s[:headLen(s)]), len(s))
	}
	if !quote {
		return s
	}
	return strconv.Quote(s)
}

// mustQuote reports whether s has to be rendered as a quoted literal.
//
// Printability is the test rather than a list of known-bad characters. A deny
// list has to be complete to work, and it is never complete: CR and ESC are
// the obvious entries, but so are NEL, the zero-width spaces, and the
// bidirectional overrides that reorder a line without changing a byte of it.
//
// Text opening with a double quote is quoted as well. Left alone it would
// render byte for byte like the quoted form of a string that really carried
// control characters, and a reader would have no way to tell which arrived.
func mustQuote(s string) bool {
	if strings.HasPrefix(s, `"`) {
		return true
	}
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}

// headLen returns the longest prefix of s at or below MaxLen that ends on a
// rune boundary, so truncation cannot manufacture invalid UTF-8 out of text
// that arrived valid.
func headLen(s string) int {
	n := MaxLen
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}
