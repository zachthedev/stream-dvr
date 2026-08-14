package retention

import (
	"slices"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// now is a fixed clock so every age is decidable.
var now = time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

// defaultPolicy matches the shipped purge weights.
var defaultPolicy = Policy{
	WatchedWeight:     3,
	AgeWeight:         1,
	RefetchableWeight: 2,
	ProtectFor:        7 * 24 * time.Hour,
}

// weeksAgo is a start time a whole number of weeks back.
func weeksAgo(weeks int) time.Time {
	return now.Add(-time.Duration(weeks) * week)
}

// complete is a finished recording old enough to be offered.
func complete(id int64) store.Recording {
	return store.Recording{
		ID:        id,
		Path:      "ExampleChannel/2026/recording.mkv",
		State:     store.StateComplete,
		Origin:    store.OriginLive,
		Bytes:     10_000_000_000,
		StartedAt: weeksAgo(4),
	}
}

// watchedAt returns a pointer to a watch time, since the field is one.
//
//go:fix inline
func watchedAt(t time.Time) *time.Time { return new(t) }

// ids reads the ranked order out, which is what most assertions are about.
func ids(candidates []Candidate) []int64 {
	out := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Recording.ID)
	}
	return out
}

// ///////////////////////////////////////////////
// Eligibility
// ///////////////////////////////////////////////

func TestRank_OffersOnlyWhatMayBePurged(t *testing.T) {
	// An exclusion is never a penalty. A pinned or protected recording
	// that merely scored low would be deleted the moment everything around
	// it scored lower, which is not what the operator asked for.
	tests := []struct {
		name    string
		mutate  func(*store.Recording)
		offered bool
	}{
		{name: "complete", mutate: func(*store.Recording) {}, offered: true},
		{name: "failed", mutate: func(r *store.Recording) { r.State = store.StateFailed }, offered: true},
		{
			name:    "parked waiting for metadata",
			mutate:  func(r *store.Recording) { r.State = store.StateAwaitingMetadata },
			offered: true,
		},
		{
			name:    "parked waiting for its file",
			mutate:  func(r *store.Recording) { r.State = store.StateAwaitingFile },
			offered: true,
		},
		{
			// The file is open and growing. Nothing may offer it.
			name:    "capturing",
			mutate:  func(r *store.Recording) { r.State = store.StateCapturing },
			offered: false,
		},
		{
			// The organizer is about to remux and rename it, and clears
			// this state itself. Offering it races a writer.
			name:    "awaiting finalize",
			mutate:  func(r *store.Recording) { r.State = store.StateAwaitingFinalize },
			offered: false,
		},
		{
			name:    "already trashed",
			mutate:  func(r *store.Recording) { r.State = store.StateTrashed },
			offered: false,
		},
		{name: "pinned", mutate: func(r *store.Recording) { r.Pinned = true }, offered: false},
		{
			name:    "younger than protect_for",
			mutate:  func(r *store.Recording) { r.StartedAt = now.Add(-24 * time.Hour) },
			offered: false,
		},
		{
			// Pinning is the operator's instruction and no score may
			// overrule it, however cheap the recording looks.
			name: "pinned and otherwise the cheapest thing here",
			mutate: func(r *store.Recording) {
				r.Pinned = true
				r.Refetchable = true
				r.WatchedAt = new(now)
				r.StartedAt = weeksAgo(52)
			},
			offered: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recording := complete(1)
			tt.mutate(&recording)

			got := Rank(defaultPolicy, []store.Recording{recording}, now)

			if offered := len(got) == 1; offered != tt.offered {
				t.Errorf("Rank() offered %d recordings, want offered = %v", len(got), tt.offered)
			}
		})
	}
}

func TestRank_ProtectForZeroProtectsNothing(t *testing.T) {
	// Zero disables the window rather than protecting everything, which is
	// how every other zero-valued limit in this project reads.
	policy := defaultPolicy
	policy.ProtectFor = 0

	recording := complete(1)
	recording.StartedAt = now.Add(-time.Minute)

	if got := Rank(policy, []store.Recording{recording}, now); len(got) != 1 {
		t.Errorf("Rank() offered %d recordings, want the minute-old one offered", len(got))
	}
}

// ///////////////////////////////////////////////
// Scoring
// ///////////////////////////////////////////////

func TestRank_Scores(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.Recording)
		want   float64
	}{
		{name: "four weeks old", mutate: func(*store.Recording) {}, want: 4},
		{
			name:   "watched adds its weight once",
			mutate: func(r *store.Recording) { r.WatchedAt = new(now) },
			want:   7,
		},
		{
			name:   "refetchable adds its weight",
			mutate: func(r *store.Recording) { r.Refetchable = true },
			want:   6,
		},
		{
			name: "every term together",
			mutate: func(r *store.Recording) {
				r.WatchedAt = new(now)
				r.Refetchable = true
			},
			want: 9,
		},
		{
			// Age is whole weeks, so a recording's position does not creep
			// every time the list is drawn.
			name:   "six days old counts no weeks",
			mutate: func(r *store.Recording) { r.StartedAt = now.Add(-6 * 24 * time.Hour) },
			want:   0,
		},
		{
			name:   "thirteen days counts one week",
			mutate: func(r *store.Recording) { r.StartedAt = now.Add(-13 * 24 * time.Hour) },
			want:   1,
		},
		{
			// A wrong clock must not push anything to the top of a list
			// that proposes deletions.
			name:   "dated in the future counts no weeks",
			mutate: func(r *store.Recording) { r.StartedAt = now.Add(52 * week) },
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recording := complete(1)
			tt.mutate(&recording)

			policy := defaultPolicy
			policy.ProtectFor = 0

			got := Rank(policy, []store.Recording{recording}, now)
			if len(got) != 1 {
				t.Fatalf("Rank() offered %d recordings, want 1", len(got))
			}
			if got[0].Score != tt.want {
				t.Errorf("Score = %v, want %v (reasons: %+v)", got[0].Score, tt.want, got[0].Reasons)
			}
		})
	}
}

func TestAgeInWeeks(t *testing.T) {
	// Tested directly because score() only ever asks for a positive count,
	// so every guard below is invisible through Rank. A helper that is
	// correct only because its one caller is careful stops being correct
	// the moment it gains a second one.
	tests := []struct {
		name  string
		start time.Time
		want  int
	}{
		{name: "started now", start: now, want: 0},
		{name: "a day short of a week", start: now.Add(-6 * 24 * time.Hour), want: 0},
		{name: "exactly a week", start: now.Add(-week), want: 1},
		{name: "an hour short of two weeks", start: now.Add(-2*week + time.Hour), want: 1},
		{name: "exactly two weeks", start: now.Add(-2 * week), want: 2},
		// A clock that jumped forward, or a row carrying a bad start, must
		// not produce a negative count that reads as a very fresh
		// recording somewhere downstream.
		{name: "dated an hour in the future", start: now.Add(time.Hour), want: 0},
		{name: "dated a year in the future", start: now.Add(52 * week), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ageInWeeks(store.Recording{StartedAt: tt.start}, now)
			if got != tt.want {
				t.Errorf("ageInWeeks(%s) = %d, want %d", tt.start.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestRank_ReasonsAccountForTheWholeScore(t *testing.T) {
	// The purge pane has to say why a file is on the list. A score with a
	// term missing from its reasons is one the operator cannot check, at
	// the moment it proposes deleting a broadcast.
	recording := complete(1)
	recording.WatchedAt = new(now)
	recording.Refetchable = true

	got := Rank(defaultPolicy, []store.Recording{recording}, now)[0]

	var summed float64
	for _, reason := range got.Reasons {
		summed += reason.Score
		if strings.TrimSpace(reason.Why) == "" {
			t.Errorf("reason %+v has no explanation", reason)
		}
	}
	if summed != got.Score {
		t.Errorf("reasons sum to %v, want the whole score %v: %+v", summed, got.Score, got.Reasons)
	}
}

func TestRank_AZeroWeightContributesNoReason(t *testing.T) {
	// An operator who zeroed a weight turned that term off. Listing it
	// with a score of 0 would say the tool considered something it did
	// not.
	recording := complete(1)
	recording.WatchedAt = new(now)
	recording.Refetchable = true

	policy := Policy{AgeWeight: 1}

	for _, reason := range Rank(policy, []store.Recording{recording}, now)[0].Reasons {
		if strings.Contains(reason.Why, "watched") || strings.Contains(reason.Why, "upstream") {
			t.Errorf("reasons carry %q, want a zeroed weight left out entirely", reason.Why)
		}
	}
}

// ///////////////////////////////////////////////
// Ordering
// ///////////////////////////////////////////////

func TestRank_OrdersIncompleteFirstThenByScore(t *testing.T) {
	// A recording that never finished has no whole broadcast behind its
	// bytes, so it goes before any finished one however the weights fall.
	cheapest := complete(1)
	cheapest.StartedAt = weeksAgo(52)
	cheapest.WatchedAt = new(now)
	cheapest.Refetchable = true

	middling := complete(2)
	middling.StartedAt = weeksAgo(10)

	failed := complete(3)
	failed.State = store.StateFailed
	failed.StartedAt = weeksAgo(2)

	got := Rank(defaultPolicy, []store.Recording{cheapest, middling, failed}, now)

	if want := []int64{3, 1, 2}; !slices.Equal(ids(got), want) {
		t.Errorf("Rank() order = %v, want %v: the unfinished capture first, then by score", ids(got), want)
	}
	if got[0].Class != ClassIncomplete {
		t.Errorf("first candidate Class = %v, want ClassIncomplete", got[0].Class)
	}
}

func TestRank_BreaksTiesLargestFirstThenByID(t *testing.T) {
	// At the cap the operator wants the most headroom for each broadcast
	// they give up. The ID settles the rest, which is what makes the list
	// stable between draws rather than merely sorted.
	small := complete(1)
	small.Bytes = 1_000_000_000

	large := complete(2)
	large.Bytes = 50_000_000_000

	same := complete(3)
	same.Bytes = 50_000_000_000

	got := Rank(defaultPolicy, []store.Recording{small, same, large}, now)

	if want := []int64{2, 3, 1}; !slices.Equal(ids(got), want) {
		t.Errorf("Rank() order = %v, want %v: larger first, then by id", ids(got), want)
	}
}

func TestRank_IsStableAcrossInputOrder(t *testing.T) {
	// The store returns rows in whatever order a query gives, and an
	// operator watching the list reorder itself between draws cannot trust
	// which row their cursor is on.
	recordings := []store.Recording{complete(1), complete(2), complete(3), complete(4)}
	recordings[1].Refetchable = true
	recordings[3].WatchedAt = new(now)

	forward := ids(Rank(defaultPolicy, recordings, now))

	reversed := slices.Clone(recordings)
	slices.Reverse(reversed)
	backward := ids(Rank(defaultPolicy, reversed, now))

	if !slices.Equal(forward, backward) {
		t.Errorf("Rank() gave %v then %v for the same rows in a different order", forward, backward)
	}
}

func TestRank_DoesNotModifyItsInput(t *testing.T) {
	// Rank only ranks. A caller holding these rows for anything else must
	// get back exactly what it passed.
	recordings := []store.Recording{complete(3), complete(1), complete(2)}
	before := slices.Clone(recordings)

	Rank(defaultPolicy, recordings, now)

	if !slices.EqualFunc(recordings, before, func(a, b store.Recording) bool { return a.ID == b.ID }) {
		t.Errorf("Rank() reordered its input to %v, want it untouched", ids(toCandidates(recordings)))
	}
}

func TestRank_EmptyInput(t *testing.T) {
	if got := Rank(defaultPolicy, nil, now); len(got) != 0 {
		t.Errorf("Rank(nil) = %v, want nothing offered", got)
	}
}

// ///////////////////////////////////////////////
// Bytes
// ///////////////////////////////////////////////

func TestBytes(t *testing.T) {
	// The pane tells the operator what a selection would free before they
	// confirm it.
	tests := []struct {
		name string
		in   []Candidate
		want int64
	}{
		{name: "nothing selected", in: nil, want: 0},
		{
			name: "one recording",
			in:   toCandidates([]store.Recording{complete(1)}),
			want: 10_000_000_000,
		},
		{
			name: "several recordings",
			in:   toCandidates([]store.Recording{complete(1), complete(2), complete(3)}),
			want: 30_000_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Bytes(tt.in); got != tt.want {
				t.Errorf("Bytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

// toCandidates wraps rows without scoring them, for assertions that are
// about a selection rather than a ranking.
func toCandidates(recordings []store.Recording) []Candidate {
	out := make([]Candidate, 0, len(recordings))
	for _, recording := range recordings {
		out = append(out, Candidate{Recording: recording})
	}
	return out
}
