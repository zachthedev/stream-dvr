// Package space decides whether a recording may start and whether one in
// flight must stop.
//
// It never deletes anything. The daemon's only automatic response to a full
// library is to stop recording and say so. Reclaiming space is the
// operator's decision, made through the assisted purge. That split is
// deliberate: an archive that quietly deletes to make room for a rerun
// destroys something irreplaceable to keep something disposable.
package space

import (
	"fmt"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Limits are the bounds a library must stay inside.
type Limits struct {
	// MaxSize caps the library's total bytes. Zero disables the cap.
	MaxSize config.Size
	// MinFree is the free space that must remain on the volume. It guards
	// the disk from the DVR when other things share it. Zero disables it.
	MinFree config.Size
}

// Usage is a library's current state.
type Usage struct {
	// LibraryBytes is every byte the library holds, counting a recording
	// that is still being written.
	//
	// Watch exists to notice a library filling during a capture that runs
	// for hours, and the capture in flight is the only thing making it
	// fill. A figure that counts finished recordings alone cannot move
	// while a broadcast records, so the max_size watermark it feeds never
	// fires. The cap then binds only between broadcasts.
	LibraryBytes int64
	// FreeBytes is the space available on the library's volume.
	FreeBytes int64
}

// Level is how close a running recording is to a limit.
type Level string

// RefusalError reports a recording that may not start.
type RefusalError struct {
	// Limit names which bound was hit.
	Limit string
	// Need is what the recording requires: its own estimate plus the
	// headroom the watermark insists on, because a capture admitted into
	// that margin is stopped on the next check. Have is the headroom left.
	Need int64
	Have int64
}

// Level values, in ascending severity.
const (
	// LevelOK means both limits have room.
	LevelOK Level = "ok"
	// LevelLow means a limit is within the warning margin. Recording
	// continues and the operator is told.
	LevelLow Level = "low"
	// LevelCritical means a limit is about to be breached. The recording
	// is finalized cleanly rather than left to fill the disk, because a
	// complete three-hour file beats a corrupt four-hour one.
	LevelCritical Level = "critical"
)

// ///////////////////////////////////////////////
// Margins
// ///////////////////////////////////////////////

// warnMargin is how much headroom triggers LevelLow, as a fraction of the
// limit. It exists so the operator hears about a filling library while
// there is still time to act.
const warnMargin = 0.10

// criticalMargin is the headroom below which a running recording is
// stopped. It is sized to hold the seconds between checks plus the
// filesystem's own slack.
const criticalMargin = 0.02

// minCriticalBytes floors the critical margin so a small cap does not
// produce a margin too thin to finalize inside.
const minCriticalBytes = int64(512 << 20)

// minWarnBytes floors the warning margin in the same proportion that
// warnMargin holds to criticalMargin, which is five to one.
//
// Flooring only the critical margin puts it above a percentage warning for
// every limit under about five gigabytes. A ladder whose lower rung sits
// above its upper rung has no lower rung, and the library steps from ok
// straight to stopping the recording.
const minWarnBytes = 5 * minCriticalBytes

// criticalCeiling and warnCeiling bound each floored margin as a share of
// the limit it is measured against, expressed as the divisor.
//
// Half for the stop and three quarters for the warning, so the two rungs
// stay apart on a small cap. Both sit far above the percentages, so neither
// binds on a library of any ordinary size and the ladder is unchanged
// there. What they prevent is a margin larger than the whole budget, which
// admits nothing at all.
const (
	criticalCeilingNum, criticalCeilingDen = 1, 2
	warnCeilingNum, warnCeilingDen         = 3, 4
)

// DefaultBitrate estimates a stream's byte rate when a channel has no
// history to draw on. It is deliberately generous: refusing a recording
// that would have fit costs one broadcast, and starting one that will not
// fit costs a disk.
const DefaultBitrate = int64(1_000_000)

// Error implements error.
func (e *RefusalError) Error() string {
	return fmt.Sprintf("%s would be breached: need %s, have %s",
		e.Limit, config.Size(e.Need), config.Size(e.Have))
}

// ///////////////////////////////////////////////
// Admission
// ///////////////////////////////////////////////

// Admit reports whether a recording of about estimate bytes may start.
//
// A recording needs its own estimate plus the margin Watch stops a capture
// inside. Admitting into that margin starts a capture the watermark ends on
// its first check, which costs a whole broadcast as a run of fragments too
// short to keep, each one leaving a failed row and a file nothing owns.
//
// Returns *RefusalError when either limit would be breached. The caller
// stops and notifies rather than making room.
func Admit(limits Limits, usage Usage, estimate int64) error {
	if estimate < 0 {
		return fmt.Errorf("estimate %d is negative", estimate)
	}

	if limits.MaxSize > 0 {
		headroom := limits.MaxSize.Bytes() - usage.LibraryBytes
		need := estimate + criticalMarginFor(limits.MaxSize.Bytes())
		if need > headroom {
			return &RefusalError{Limit: "library max_size", Need: need, Have: max(headroom, 0)}
		}
	}

	if limits.MinFree > 0 {
		headroom := usage.FreeBytes - limits.MinFree.Bytes()
		need := estimate + criticalMarginFor(limits.MinFree.Bytes())
		if need > headroom {
			return &RefusalError{Limit: "volume min_free", Need: need, Have: max(headroom, 0)}
		}
	}
	return nil
}

// Watch reports how close a running recording is to a limit.
//
// It is checked periodically during capture, because a broadcast can run
// for hours and a library that fit at the start need not fit at the end.
func Watch(limits Limits, usage Usage) Level {
	level := LevelOK

	if limits.MaxSize > 0 {
		level = worse(level, levelFor(limits.MaxSize.Bytes()-usage.LibraryBytes, limits.MaxSize.Bytes()))
	}
	if limits.MinFree > 0 {
		// The floor's headroom is measured against the floor itself, so a
		// large floor is not treated as a large allowance.
		level = worse(level, levelFor(usage.FreeBytes-limits.MinFree.Bytes(), limits.MinFree.Bytes()))
	}
	return level
}

// criticalMarginFor returns the headroom a limit reserves for finishing the
// recording it is about to stop.
//
// Admission and the watermark both measure against it, so the two cannot
// disagree about how much room a capture needs.
func criticalMarginFor(limit int64) int64 {
	return capMargin(max(int64(float64(limit)*criticalMargin), minCriticalBytes),
		limit, criticalCeilingNum, criticalCeilingDen)
}

// capMargin keeps a floored margin inside the limit it is measured against.
//
// The floors are absolute, and headroom under a size cap is bounded above
// by the cap itself. Left uncapped, a 500 MB library is critical while
// still empty and admits nothing at all: the operator gets a refusal
// naming a figure larger than the budget they set, every capture stops,
// and the trash is released early on a library holding nothing.
//
// The two rungs take different shares, so a capped ladder is still a
// ladder. Capping both at the same fraction makes the warning and the stop
// the same number, and a library steps from ok straight to stopping the
// recording with nothing in between.
func capMargin(margin, limit, num, den int64) int64 {
	if limit <= 0 {
		return margin
	}
	return min(margin, limit*num/den)
}

// levelFor classifies headroom against the size of the limit it belongs to.
func levelFor(headroom, limit int64) Level {
	critical := criticalMarginFor(limit)
	warn := capMargin(max(int64(float64(limit)*warnMargin), minWarnBytes),
		limit, warnCeilingNum, warnCeilingDen)

	if headroom <= critical {
		return LevelCritical
	}
	if headroom <= warn {
		return LevelLow
	}
	return LevelOK
}

// worse returns the more severe of two levels.
func worse(a, b Level) Level {
	if severity(b) > severity(a) {
		return b
	}
	return a
}

// severity ranks a level for comparison.
func severity(l Level) int {
	switch l {
	case LevelCritical:
		return 2
	case LevelLow:
		return 1
	default:
		return 0
	}
}

// ///////////////////////////////////////////////
// Estimation
// ///////////////////////////////////////////////

// Estimate returns the bytes a broadcast of the given length is expected to
// consume at a byte rate.
//
// A zero or negative rate falls back to DefaultBitrate, so a channel with
// no history still gets a usable figure rather than an admission check that
// always passes.
func Estimate(bytesPerSecond int64, duration time.Duration) int64 {
	if bytesPerSecond <= 0 {
		bytesPerSecond = DefaultBitrate
	}
	if duration <= 0 {
		return 0
	}
	return int64(duration.Seconds() * float64(bytesPerSecond))
}

// Bitrate returns the byte rate a finished recording achieved, or zero when
// it cannot be derived.
func Bitrate(bytes int64, duration time.Duration) int64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	return int64(float64(bytes) / duration.Seconds())
}
