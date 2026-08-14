// Package record captures live broadcasts to disk.
//
// Capture is deliberately ignorant of metadata. It writes to a filename
// derived only from the platform, the channel, and the start time, all of
// which are known before a single byte arrives. Naming happens afterward,
// in the organizer, from metadata that has been validated. A metadata
// failure can therefore delay a rename but can never truncate, misname, or
// lose a recording.
//
// Engine is an interface, so everything above it depends on the capture
// contract rather than on the subprocess behind it.
package record

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/naming"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Engine captures a live stream to a file.
type Engine interface {
	// Probe reports whether a URL is currently broadcasting and what
	// metadata it exposes.
	Probe(ctx context.Context, url string) (Probe, error)

	// Capture records until the broadcast ends or ctx is cancelled. It
	// returns a Result describing the attempt even for a failed capture,
	// because bytes already on disk are still worth keeping.
	Capture(ctx context.Context, req Request) (Result, error)
}

// Probe is what a liveness check learned.
type Probe struct {
	// Live reports whether the channel is broadcasting now.
	Live bool
	// Qualities lists the stream names the source offers, such as
	// "1080p60" and "best".
	Qualities []string
	// Metadata is what the source says about the broadcast. Any field can
	// be empty, which is exactly why naming happens later.
	Metadata Metadata
}

// Metadata is a broadcast's description at one moment.
type Metadata struct {
	// ID is the platform's identifier for this broadcast, used to
	// deduplicate a broadcast discovered more than once.
	ID string
	// Author is the channel's display name.
	Author string
	// Category is the game or section the broadcast is filed under.
	Category string
	// Title is the broadcast title.
	Title string
}

// Request describes one capture.
type Request struct {
	// URL is the channel or stream address.
	URL string
	// Qualities is the quality ladder, tried in order.
	Qualities []string
	// Output is the absolute path to write, which must not exist.
	Output string
	// LogPath receives the engine's own diagnostics. Empty discards them.
	LogPath string
}

// Result describes a finished capture.
type Result struct {
	// Bytes is the size of the captured file.
	Bytes int64
	// SizeUnknown reports that the output could not be measured, so Bytes
	// is a zero nobody established rather than a capture that wrote
	// nothing.
	//
	// It exists because the two are told apart nowhere else. A confident
	// zero on a row that never reaches the library is never corrected, so
	// the file sits on the volume while the space budget reads the library
	// smaller than it is.
	SizeUnknown bool
	// StartedAt and EndedAt bound the capture.
	StartedAt time.Time
	EndedAt   time.Time
	// ExitCode is the engine's exit status. Zero means the broadcast
	// ended normally.
	ExitCode int
}

// ///////////////////////////////////////////////
// Capture naming
// ///////////////////////////////////////////////

// maxChannelSegment bounds the channel portion of a capture filename. A
// generic URL source has no short name, and its full address would produce
// a filename no filesystem accepts.
const maxChannelSegment = 40

// channelDigestLength is how much of a channel's digest is appended when
// its name did not reach the filename intact. Eight hex characters
// separate every channel a library will hold.
const channelDigestLength = 8

// CaptureExtension is the container capture always writes.
//
// MPEG-TS survives truncation: a machine that reboots mid-broadcast leaves
// a file that still plays, where a half-written MP4 does not. Remuxing to
// the final container happens after the broadcast ends.
const CaptureExtension = "ts"

// Duration returns how long the capture ran.
func (r Result) Duration() time.Duration {
	return r.EndedAt.Sub(r.StartedAt)
}

// CaptureName returns the filename capture writes to, relative to the
// incoming directory.
func CaptureName(platform, channel string, start time.Time) string {
	return CaptureStem(platform, channel, start) + "." + CaptureExtension
}

// CaptureStem returns a capture's filename without its extension.
//
// Every input is known before capture starts, so this name never depends on
// a network call that might fail. The start time in Unix seconds keeps it
// unique per channel without consulting the filesystem.
//
// Backfill writes the same stem under an extension it does not choose,
// because a downloaded broadcast arrives as whatever the platform served.
// One definition of the stem sanitizes a channel name once, so a capture
// and its backfilled replacement cannot land on paths that disagree.
func CaptureStem(platform, channel string, start time.Time) string {
	// Whatever the name loses on the way to a filename, the digest carries,
	// so two channels captured in the same second cannot land on one file.
	// Two URL sources differing only past the fortieth character, and two
	// names that both sanitize away, are the cases that reach it.
	safe, err := naming.SanitizeSegment(channel)
	if err != nil {
		safe = "channel-" + digest(channel)
	}
	if len([]rune(safe)) > maxChannelSegment {
		safe = string([]rune(safe)[:maxChannelSegment]) + "-" + digest(channel)
	}

	platformSafe, err := naming.SanitizeSegment(platform)
	if err != nil {
		platformSafe = "unknown"
	}

	return fmt.Sprintf("%s-%s-%d",
		strings.ToLower(platformSafe), strings.ToLower(safe), start.UTC().Unix())
}

// digest returns a short stable fingerprint of a channel identifier.
func digest(channel string) string {
	sum := sha256.Sum256([]byte(channel))
	return hex.EncodeToString(sum[:])[:channelDigestLength]
}
