package importer

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// measurement is what the file itself says about its size and its length.
type measurement struct {
	// bytes is the size on disk.
	bytes int64
	// media is the length read out of the container, or zero when nothing
	// could read one.
	media time.Duration
}

// ///////////////////////////////////////////////
// Tolerances
// ///////////////////////////////////////////////

// lengthSlack is how far a sidecar's length may sit from the measured one
// before it counts as a disagreement.
//
// A remux rewrites container timing, and a length taken from a clock around a
// capture is not the length of the media it produced. A second either way is
// two records of the same recording, not two different recordings.
const lengthSlack = time.Second

// maxSidecarBytes bounds what is read from one sidecar.
//
// A real sidecar is kilobytes: a title, a few timestamps, and a handful of
// gaps. The bound exists because the scan reads every file named like one,
// and a file named like a sidecar is not necessarily a record.
const maxSidecarBytes = 1 << 20

// maxSidecarGaps bounds how many holes one sidecar may restore.
//
// Each gap is its own insert, and a filled one is an insert and an update, so
// an unbounded list turns one file into hundreds of thousands of writes. A
// broadcast interrupted a thousand times is already a recording nobody would
// keep.
const maxSidecarGaps = 1000

// maxSidecarTitles bounds how many title observations one sidecar may
// restore, for the same reason as maxSidecarGaps: each one is its own write
// transaction. A poller sampling every few minutes leaves tens over a long
// broadcast, so this is far above anything a recorder produces.
const maxSidecarTitles = 1000

// broadcastOverlap is how far from a start time a restore looks for a
// broadcast this library already holds. It matches the window the store
// merges discoveries within, so a restore joins whatever an ordinary
// discovery of the same broadcast would have joined.
const broadcastOverlap = 15 * time.Minute

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

// errSidecarTooNew reports a sidecar written by a later build.
var errSidecarTooNew = errors.New("written by a newer build")

// errSidecarUnusable reports a sidecar this build refuses to act on, for a
// reason its own contents settle rather than its version.
var errSidecarUnusable = errors.New("not a record this build will act on")

// ///////////////////////////////////////////////
// Media files
// ///////////////////////////////////////////////

// isMedia reports whether a path is one of the containers this project
// produces.
//
// config.Containers is the one list, shared with the setting that chooses
// which of them a recording is remuxed into. A separate list here would let
// an operator configure a container the import then walks past.
func isMedia(relPath string) bool {
	base := path.Base(relPath)
	ext := path.Ext(base)
	// A name that is nothing but an extension is a hidden file, not a
	// recording: path.Ext reads ".mkv" as all extension and no stem.
	if strings.TrimSuffix(base, ext) == "" {
		return false
	}
	return slices.Contains(config.Containers, strings.ToLower(strings.TrimPrefix(ext, ".")))
}

// reserved reports a directory the library keeps for itself.
//
// paths.ReservedDirNames is the same list the name renderer refuses to file a
// recording into, so what an import walks past and what a rendered name may
// never become are one definition. The trash sits inside the state directory,
// so skipping that covers it.
func reserved(relPath string) bool {
	head, _, _ := strings.Cut(relPath, "/")
	return slices.ContainsFunc(paths.ReservedDirNames, func(name string) bool {
		return strings.EqualFold(head, name)
	})
}

// fold normalizes a channel name for comparison.
func fold(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// pathKey normalizes a library-relative path so two spellings of one file
// land on one key. See recordedPaths for why case is folded.
func pathKey(relPath string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(relPath)))
}

// join glues two reasons, dropping whichever is empty.
func join(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	}
	return first + "; " + second
}

// ///////////////////////////////////////////////
// Reconciling a sidecar against its file
// ///////////////////////////////////////////////

// claimedOrMeasured prefers the duration a sidecar recorded, falling back to
// the measured length.
//
// The sidecar's duration is a clock around the capture and the measurement is
// the media, and they answer different questions: a capture that dropped a
// stretch ran longer than the footage it produced. Where the sidecar has one,
// it is the better answer. Where it has none, a measured length beats nothing.
func claimedOrMeasured(claimedMS int64, measured time.Duration) time.Duration {
	if claimed := time.Duration(claimedMS) * time.Millisecond; claimed > 0 {
		return claimed
	}
	return measured
}

// disagreements names what a sidecar claims that its file contradicts,
// sorted.
//
// The file wins every one of these, and the operator is told. A sidecar is a
// record of what a build believed when it wrote it, and builds have written
// wrong ones: the size and both lengths land at zero if the row was stamped
// before the file was measured.
//
// A claim of zero is not a disagreement. Zero is how this project spells
// nobody measured it, and reporting it as a contradiction would put a warning
// beside every recording an older build wrote.
func disagreements(sidecar organize.Sidecar, measured measurement) []string {
	var found []string

	if sidecar.Bytes > 0 && sidecar.Bytes != measured.bytes {
		found = append(found, fmt.Sprintf("its sidecar claims %s and the file holds %s",
			config.Size(sidecar.Bytes), config.Size(measured.bytes)))
	}

	claimed := time.Duration(sidecar.MediaDurationMS) * time.Millisecond
	if claimed > 0 && measured.media > 0 && absDiff(claimed, measured.media) > lengthSlack {
		found = append(found, fmt.Sprintf("its sidecar claims %s of media and the file holds %s",
			claimed.Round(time.Second), measured.media.Round(time.Second)))
	}

	// The capture duration is checked too, and only against a file that is
	// longer than it claims. A capture legitimately runs longer than the
	// footage it produced, because a dropped stretch costs media and not
	// clock, so a shortfall is ordinary. Media the clock cannot account for
	// is not: the recording holds more than the row says was ever recorded.
	wall := time.Duration(sidecar.DurationMS) * time.Millisecond
	if wall > 0 && measured.media > wall+lengthSlack {
		found = append(found, fmt.Sprintf("its sidecar claims a %s capture and the file holds %s",
			wall.Round(time.Second), measured.media.Round(time.Second)))
	}

	slices.Sort(found)
	return found
}

// absDiff returns the distance between two durations.
func absDiff(a, b time.Duration) time.Duration {
	if a > b {
		return a - b
	}
	return b - a
}

// lossyNote says which fields a filename could not carry back.
func lossyNote(match naming.Match) string {
	var notes []string
	if len(match.Lossy) > 0 {
		notes = append(notes, fmt.Sprintf("%s cannot be read back exactly, because the "+
			"rendering replaced characters it cannot undo", strings.Join(match.Lossy, " and ")))
	}
	if match.Duplicate > 0 {
		notes = append(notes, fmt.Sprintf("it carried the deduplication suffix (%d), "+
			"which is not part of the title", match.Duplicate))
	}
	return strings.Join(notes, "; ")
}

// dispositionFor turns a failed write into what it means for one file.
//
// A path another row claimed between the scan and the write is a skip rather
// than a failure. Two imports over one directory is the ordinary way that
// happens, and the second one finding the work already done is success.
func dispositionFor(relPath string, err error) File {
	if errors.Is(err, store.ErrDuplicatePath) {
		return File{
			Path:        relPath,
			Disposition: Skipped,
			Reason:      "a recording claimed this file while the scan was running",
		}
	}
	return File{Path: relPath, Disposition: Refused, Reason: err.Error()}
}

// ///////////////////////////////////////////////
// Counting
// ///////////////////////////////////////////////

// Count returns how many files ended in one disposition.
func (r Report) Count(disposition Disposition) int {
	var n int
	for _, file := range r.Files {
		if file.Disposition == disposition {
			n++
		}
	}
	return n
}

// Imported returns how many rows the run created.
func (r Report) Imported() int {
	return r.Count(Restored) + r.Count(Inferred)
}
