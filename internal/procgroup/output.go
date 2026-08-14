package procgroup

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Output collects the leading bytes a tool writes and counts the rest.
//
// A tool's output is read into memory to be parsed as a short answer or
// quoted into an error, and nothing about the tool bounds how much of it
// there is. Reading without a limit lets a program on PATH size this one:
// a body of a few tens of megabytes costs several times that in the
// allocations that carry it through a read, a copy, and a parse.
//
// Truncation is recorded rather than hidden, because a prefix of a JSON
// body parses as a smaller valid document. A capture that read half a
// stream listing would report a broadcasting channel as offline.
type Output struct {
	limit   int
	kept    []byte
	dropped int
	// tail keeps the closing bytes instead of the opening ones, for a tool
	// that states its answer after the work rather than before it.
	tail bool
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// MaxErrorText bounds how much of a tool's output an error may carry,
// because an error built from it reaches a log line and a notification
// body.
const MaxErrorText = 2 << 10

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// NewOutput returns a writer that keeps at most limit bytes.
func NewOutput(limit int) *Output {
	return &Output{limit: limit}
}

// NewTailOutput returns a writer that keeps at most the last limit bytes.
//
// It is for a tool that reports its answer after doing the work, where the
// bytes worth keeping are the ones a leading capture discards. A decoder
// warning repeated for every frame of a damaged file fills any bound long
// before the measurement at the end is written.
func NewTailOutput(limit int) *Output {
	return &Output{limit: limit, tail: true}
}

// Write implements io.Writer, keeping at most the configured limit.
//
// It never reports a short write. A tool blocked on its own stdout is a
// tool that stops doing the work it was started for.
func (o *Output) Write(p []byte) (int, error) {
	if o.tail {
		o.kept = append(o.kept, p...)
		// Clamped, because this writer is handed to a subprocess and driven
		// from the copy goroutine os/exec owns. A slice out of range there
		// is unrecoverable and takes the whole process with it.
		if over := min(len(o.kept)-o.limit, len(o.kept)); over > 0 {
			// Copied down rather than resliced, so the discarded head is
			// reclaimed instead of held for the life of the capture.
			o.kept = append(o.kept[:0], o.kept[over:]...)
			o.dropped += over
		}
		return len(p), nil
	}

	room := max(min(o.limit-len(o.kept), len(p)), 0)
	o.kept = append(o.kept, p[:room]...)
	o.dropped += len(p) - room
	return len(p), nil
}

// Bytes returns the text collected so far.
func (o *Output) Bytes() []byte {
	return o.kept
}

// String returns the text collected so far.
func (o *Output) String() string {
	return string(o.kept)
}

// Dropped returns how many bytes were discarded past the limit.
func (o *Output) Dropped() int {
	return o.dropped
}

// Truncated reports whether the tool wrote more than the limit.
//
// A caller parsing this output must treat that as a failure. The answer it
// would parse is a prefix of the real one, and a prefix that still parses
// is worse than no answer at all.
func (o *Output) Truncated() bool {
	return o.dropped > 0
}

// Excerpt returns the collected text cut to limit bytes, saying how much
// was left out.
//
// Errors built from a tool's output reach a log line and a notification
// body, neither of which is a place for a megabyte of diagnostics.
// A tail capture is excerpted from its own end. It keeps the closing bytes
// because that is where the tool states its answer, and cutting the opening
// of what survived would report the middle of the stream and drop the
// reason the run failed.
func (o *Output) Excerpt(limit int) string {
	if o.tail {
		return excerptTail(o.String(), o.Dropped(), limit)
	}
	return Excerpt(o.String(), o.Dropped(), limit)
}

// excerptTail cuts text to its last limit bytes, saying how much was left
// out.
func excerptTail(text string, omitted, limit int) string {
	text = strings.TrimSpace(text)

	if len(text) > limit {
		cut := len(text) - limit
		// Cutting mid-rune leaves a replacement character in a message that
		// is otherwise the tool's own words.
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		omitted += cut
		text = text[cut:]
	}
	if omitted > 0 {
		return fmt.Sprintf("(%d earlier bytes) %s", omitted, text)
	}
	return text
}

// Excerpt cuts text to limit bytes, saying how much of it and of omitted
// was left out.
//
// omitted is what the caller already knows was discarded before the text
// reached it, so one count covers both losses.
func Excerpt(text string, omitted, limit int) string {
	text = strings.TrimSpace(text)

	if len(text) > limit {
		cut := limit
		// Cutting mid-rune leaves a replacement character in a message
		// that is otherwise the tool's own words.
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		omitted += len(text) - cut
		text = text[:cut]
	}

	if omitted > 0 {
		return fmt.Sprintf("%s (%d more bytes)", text, omitted)
	}
	return text
}
