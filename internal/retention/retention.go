// Package retention scores which recordings are the cheapest to lose.
//
// It only ranks. Nothing here deletes, moves, or touches a file, and no
// caller may treat a ranking as permission to act. Every deletion in this
// project starts with a keypress and ends with one confirmation. Keeping
// the scoring pure is what makes that separation checkable rather than a
// promise, and it is why this package takes rows and returns rows.
//
// The score answers one question: if a broadcast has to go, which one
// costs least? Higher scores are cheaper to lose and sort first.
package retention

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Class separates a recording that is finished from one that is not.
//
// It is not part of the score. The operator configures three weights and
// can see all three, and a hidden fourth term that outranked them would
// make the list unexplainable. Ordering by class first says the same
// thing where the operator can read it.
type Class int

// Policy is the scoring the operator configured.
//
// It mirrors the purge weights rather than taking the config type, the
// way space.Limits does. The scoring is testable without a config file,
// and a rename in the config shape is not a change to what ranking means.
type Policy struct {
	// WatchedWeight is added once a recording has been watched.
	WatchedWeight float64
	// AgeWeight is added per full week of age.
	AgeWeight float64
	// RefetchableWeight is added when the broadcast still exists
	// upstream, because deleting a copy that can be fetched again is the
	// cheapest deletion available.
	RefetchableWeight float64
	// ProtectFor is how recent a recording must be to be excluded
	// outright. It is an exclusion rather than a penalty: an operator who
	// says "not this week" must not be overruled by an old enough score.
	ProtectFor time.Duration
}

// Reason names one term that contributed to a score.
//
// The list exists so the purge pane can say why a file is on it. A
// ranking an operator cannot interrogate is one they have to trust
// blindly at the moment it proposes deleting something.
type Reason struct {
	// Why is the term, in the operator's words.
	Why string
	// Score is what this term contributed.
	Score float64
}

// Candidate is a recording the operator may choose to purge.
type Candidate struct {
	// Recording is the row as it stands.
	Recording store.Recording
	// Class orders incomplete recordings ahead of finished ones.
	Class Class
	// Score is the sum of every reason. Higher is cheaper to lose.
	Score float64
	// Reasons are the terms that produced the score, in the order they
	// were applied.
	Reasons []Reason
}

// ///////////////////////////////////////////////
// Classes
// ///////////////////////////////////////////////

const (
	// ClassIncomplete is a recording that never reached the library: a
	// failed capture, or one parked waiting for metadata or for another
	// program to release its file. Its bytes are on disk and no finished
	// broadcast is behind them, so it is offered first.
	ClassIncomplete Class = iota
	// ClassComplete is a finished, verified recording. Losing one costs a
	// whole broadcast.
	ClassComplete
)

// week is the age granularity the operator configures against. Age is
// counted in whole weeks so a recording's position does not creep every
// time the list is drawn.
const week = 7 * 24 * time.Hour

// offerable maps each state a recording may be purged from to its class.
//
// It is the one definition. classify reads it and Offerable lists it, so a
// state added here reaches both the ranking and the query that feeds it.
// StateCapturing is absent because the file is open and growing.
// StateAwaitingFinalize is absent because the organizer is about to remux
// and rename it, and it clears that state on its own. The two parked states
// can sit indefinitely on metadata that never arrives or a program that
// never lets go, so they are offered.
var offerable = map[store.State]Class{
	store.StateComplete:         ClassComplete,
	store.StateFailed:           ClassIncomplete,
	store.StateAwaitingMetadata: ClassIncomplete,
	store.StateAwaitingFile:     ClassIncomplete,
}

// incompleteWhy states why a recording is offered ahead of the scored
// ones, in terms of the state it is actually in.
func incompleteWhy(state store.State) string {
	switch state {
	case store.StateAwaitingMetadata:
		return "waiting on a title that never arrived, so it is filed under its capture name"
	case store.StateAwaitingFile:
		return "waiting on another program to release the file, so it never moved into place"
	default:
		return "never finished, so no whole broadcast is behind these bytes"
	}
}

// Rank orders recordings by how cheap they are to lose, cheapest first.
//
// Excluded outright, never merely penalized: a pinned recording, one
// younger than ProtectFor, one already in the trash, and one the recorder
// or the organizer is still working on. The first two are the operator's
// own instructions and no score may overrule them. The last two would
// race a writer.
//
// The input is not modified and the result is fully ordered, so the same
// rows in any order produce the same list.
func Rank(policy Policy, recordings []store.Recording, now time.Time) []Candidate {
	candidates := make([]Candidate, 0, len(recordings))
	for _, recording := range recordings {
		if !Purgeable(recording) || protected(policy, recording, now) {
			continue
		}
		class, _ := classify(recording)
		candidates = append(candidates, score(policy, recording, class, now))
	}

	slices.SortFunc(candidates, func(a, b Candidate) int {
		if by := cmp.Compare(a.Class, b.Class); by != 0 {
			return by
		}
		if by := cmp.Compare(b.Score, a.Score); by != 0 {
			return by
		}
		// At the cap the operator wants the most headroom per broadcast
		// lost, so the larger file wins a tie. ID settles the rest, which
		// is what makes the order stable rather than merely sorted.
		if by := cmp.Compare(b.Recording.Bytes, a.Recording.Bytes); by != 0 {
			return by
		}
		return cmp.Compare(a.Recording.ID, b.Recording.ID)
	})
	return candidates
}

// Purgeable reports whether a recording may be purged at all, ignoring
// every question of score.
//
// It covers only what the row itself decides: the state, and the pin the
// operator set. The protect_for window is policy and belongs to Rank.
//
// It is exported because the purge has to ask twice. Rank asks to build the
// list, and the purge asks again at the moment it acts. A ranking is a
// snapshot: between drawing the list and confirming it, the recorder can
// start writing to that row and the operator can pin it from another pane.
func Purgeable(recording store.Recording) bool {
	if recording.Pinned {
		return false
	}
	_, ok := classify(recording)
	return ok
}

// Offerable lists the states a purge candidate can be in, sorted.
//
// It exists so a caller can ask the store for exactly the rows that may
// appear on a purge list, rather than scanning the whole library and
// discarding most of it. Naming those states at the query instead would put
// a second copy of Purgeable's rule where nothing checks it against this one.
func Offerable() []store.State {
	states := slices.Collect(maps.Keys(offerable))
	slices.Sort(states)
	return states
}

// Bytes sums what a selection would free.
func Bytes(candidates []Candidate) int64 {
	var total int64
	for _, candidate := range candidates {
		total += candidate.Recording.Bytes
	}
	return total
}

// classify reports a recording's class, and whether it may be offered at
// all.
func classify(recording store.Recording) (Class, bool) {
	class, ok := offerable[recording.State]
	return class, ok
}

// protected reports whether a recording is too recent to offer.
func protected(policy Policy, recording store.Recording, now time.Time) bool {
	if policy.ProtectFor <= 0 {
		return false
	}
	return now.Sub(recording.StartedAt) < policy.ProtectFor
}

// score applies every weight the policy carries and records what each one
// contributed.
func score(policy Policy, recording store.Recording, class Class, now time.Time) Candidate {
	candidate := Candidate{Recording: recording, Class: class}

	if class == ClassIncomplete {
		// Carried as a reason with no score so the pane can say why this
		// is at the top. The ordering is the class, not a number.
		//
		// Stated per state, because the class holds two different things
		// and the operator acts on the words. A capture that failed has no
		// whole broadcast behind it. A capture waiting on a title or on a
		// program to let go of its file is intact, and telling somebody it
		// never finished invites them to delete a recording that is only
		// parked.
		candidate.Reasons = append(candidate.Reasons, Reason{Why: incompleteWhy(recording.State)})
	}

	if recording.WatchedAt != nil && policy.WatchedWeight != 0 {
		candidate.Score += policy.WatchedWeight
		candidate.Reasons = append(candidate.Reasons,
			Reason{Why: "already watched", Score: policy.WatchedWeight})
	}

	if weeks := ageInWeeks(recording, now); weeks > 0 && policy.AgeWeight != 0 {
		points := policy.AgeWeight * float64(weeks)
		candidate.Score += points
		candidate.Reasons = append(candidate.Reasons,
			Reason{Why: plural(weeks) + " old", Score: points})
	}

	if recording.Refetchable && policy.RefetchableWeight != 0 {
		candidate.Score += policy.RefetchableWeight
		candidate.Reasons = append(candidate.Reasons,
			Reason{Why: "still available upstream", Score: policy.RefetchableWeight})
	}

	return candidate
}

// ageInWeeks counts whole weeks since a recording started. A recording
// dated in the future counts as no weeks rather than negative ones, so a
// wrong clock cannot push something to the top of the list.
func ageInWeeks(recording store.Recording, now time.Time) int {
	age := now.Sub(recording.StartedAt)
	if age < week {
		return 0
	}
	return int(age / week)
}

// plural renders a week count.
func plural(weeks int) string {
	if weeks == 1 {
		return "1 week"
	}
	return strconv.Itoa(weeks) + " weeks"
}
