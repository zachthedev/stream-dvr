package store

import (
	"errors"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Test helpers
// ///////////////////////////////////////////////

// fetchLease is the window a claim is held for in these tests. Long enough
// that nothing expires unless a test moves the clock itself.
const fetchLease = time.Hour

// newBroadcastFor seeds a broadcast to hang a fetch on, because the table
// has a foreign key onto it.
func newBroadcastFor(t *testing.T, store *Store, channelID int64, start time.Time) int64 {
	t.Helper()

	broadcast, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channelID,
		StartedAt: start,
		Source:    SourceAPI,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	return broadcast.ID
}

// ///////////////////////////////////////////////
// ClaimFetch
// ///////////////////////////////////////////////

func TestClaimFetch_TakesAnUnclaimedBroadcast(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	claimed, err := store.ClaimFetch(broadcastID, 1, now, fetchLease)
	if err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if !claimed {
		t.Error("ClaimFetch() = false for an untouched broadcast, want true")
	}
}

func TestClaimFetch_RefusesOneAnotherFetcherHolds(t *testing.T) {
	// The guard the whole design rests on. Two fetchers downloading one VOD
	// race each other onto one path, and the loser's work is thrown away
	// after spending the bandwidth.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	first, err := store.ClaimFetch(broadcastID, 1, now, fetchLease)
	if err != nil || !first {
		t.Fatalf("first ClaimFetch() = %v, %v; want true, nil", first, err)
	}

	second, err := store.ClaimFetch(broadcastID, 2, now.Add(time.Minute), fetchLease)
	if err != nil {
		t.Fatalf("second ClaimFetch() err = %v, want nil", err)
	}
	if second {
		t.Error("ClaimFetch() = true while another fetcher holds it, want false")
	}
}

func TestClaimFetch_ReclaimsAfterTheLeaseExpires(t *testing.T) {
	// A fetcher that crashes mid-download cannot release what it holds.
	// Without an expiry that broadcast is unfetchable for good.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("first ClaimFetch() err = %v, want nil", err)
	}

	later := now.Add(fetchLease).Add(time.Minute)
	reclaimed, err := store.ClaimFetch(broadcastID, 2, later, fetchLease)
	if err != nil {
		t.Fatalf("second ClaimFetch() err = %v, want nil", err)
	}
	if !reclaimed {
		t.Error("ClaimFetch() = false past the lease, want the stale claim taken over")
	}
}

func TestClaimFetch_NeverTakesATerminalBroadcast(t *testing.T) {
	// A private or removed video answers the same way forever. A timer
	// that kept asking would spend a request per pass for good.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	// A zero retry time is what marks a failure as never worth repeating.
	if err := store.RecordFetchFailure(broadcastID, "video is private", now, time.Time{}); err != nil {
		t.Fatalf("RecordFetchFailure() err = %v, want nil", err)
	}

	claimed, err := store.ClaimFetch(broadcastID, 1, now.Add(24*time.Hour), fetchLease)
	if err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if claimed {
		t.Error("ClaimFetch() = true for a terminal broadcast, want it left alone")
	}
}

func TestClaimFetch_NeverTakesABroadcastAlreadyInTheLibrary(t *testing.T) {
	// A done broadcast has a recording on disk. Claiming it spends the
	// bandwidth of a full download to write a file that is already there.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if err := store.ReleaseFetch(broadcastID, FetchDone, now); err != nil {
		t.Fatalf("ReleaseFetch() err = %v, want nil", err)
	}

	claimed, err := store.ClaimFetch(broadcastID, 1, now.Add(24*time.Hour), fetchLease)
	if err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if claimed {
		t.Error("ClaimFetch() = true for a broadcast already fetched, want it left alone")
	}
}

func TestClaimFetch_DoesNotResurrectAReleasedRowThroughAnOldBackoff(t *testing.T) {
	// The row shape the hole needs: a next_attempt_at in the past from an
	// earlier failure, and a claimed_at the release cleared. Every timing
	// term is satisfied, so the state is the only thing refusing the claim.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if err := store.RecordFetchFailure(broadcastID, "a transient failure", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("RecordFetchFailure() err = %v, want nil", err)
	}

	retried, err := store.ClaimFetch(broadcastID, 1, now.Add(time.Hour), fetchLease)
	if err != nil {
		t.Fatalf("retry ClaimFetch() err = %v, want nil", err)
	}
	if !retried {
		t.Fatal("ClaimFetch() = false once the backoff elapsed, want the retry taken")
	}

	if err := store.ReleaseFetch(broadcastID, FetchDone, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("ReleaseFetch() err = %v, want nil", err)
	}

	claimed, err := store.ClaimFetch(broadcastID, 1, now.Add(24*time.Hour), fetchLease)
	if err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if claimed {
		t.Error("ClaimFetch() = true for a done row whose old backoff had elapsed, want it left alone")
	}
}

func TestClaimFetch_WaitsForTheBackoffToElapse(t *testing.T) {
	// Retrying before the backoff has run is the same as having no backoff.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	retryAt := now.Add(30 * time.Minute)
	if err := store.RecordFetchFailure(broadcastID, "connection reset", now, retryAt); err != nil {
		t.Fatalf("RecordFetchFailure() err = %v, want nil", err)
	}

	if early, err := store.ClaimFetch(broadcastID, 1, retryAt.Add(-time.Minute), fetchLease); err != nil {
		t.Fatalf("early ClaimFetch() err = %v, want nil", err)
	} else if early {
		t.Error("ClaimFetch() = true before the backoff elapsed, want false")
	}

	if due, err := store.ClaimFetch(broadcastID, 1, retryAt, fetchLease); err != nil {
		t.Fatalf("due ClaimFetch() err = %v, want nil", err)
	} else if !due {
		t.Error("ClaimFetch() = false once the backoff elapsed, want true")
	}
}

func TestClaimFetch_CountsAttempts(t *testing.T) {
	// The attempt count is what a cap is applied to, so a claim that did
	// not count would make the cap unreachable.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	for i := range 3 {
		at := now.Add(time.Duration(i) * 2 * fetchLease)
		if _, err := store.ClaimFetch(broadcastID, 1, at, fetchLease); err != nil {
			t.Fatalf("ClaimFetch() %d err = %v, want nil", i, err)
		}
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", fetch.Attempts)
	}
}

func TestClaimFetch_RejectsANonPositiveLease(t *testing.T) {
	// A zero lease makes every claim instantly stale, so two fetchers both
	// take the same broadcast and the guard silently stops guarding.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, 0); err == nil {
		t.Error("ClaimFetch() err = nil for a zero lease, want a rejection")
	}
}

// ///////////////////////////////////////////////
// ReleaseFetch
// ///////////////////////////////////////////////

func TestReleaseFetch_DropsTheClaim(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if err := store.ReleaseFetch(broadcastID, FetchDone, now.Add(time.Minute)); err != nil {
		t.Fatalf("ReleaseFetch() err = %v, want nil", err)
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.State != FetchDone {
		t.Errorf("State = %q, want %q", fetch.State, FetchDone)
	}
	if !fetch.ClaimedAt.IsZero() {
		t.Errorf("ClaimedAt = %v, want the claim cleared", fetch.ClaimedAt)
	}
}

func TestReleaseFetch_ReportsABroadcastNobodyClaimed(t *testing.T) {
	// Releasing what was never claimed means the caller's bookkeeping and
	// the database's disagree, and silence would hide that.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	err := store.ReleaseFetch(broadcastID, FetchDone, now)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ReleaseFetch() err = %v, want ErrNotFound", err)
	}
}

// ///////////////////////////////////////////////
// FetchFor
// ///////////////////////////////////////////////

func TestFetchFor_ReportsABroadcastNeverTried(t *testing.T) {
	// "Never attempted" and "attempted, no state" are different answers,
	// and a caller acts differently on each.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.FetchFor(broadcastID); !errors.Is(err, ErrNotFound) {
		t.Errorf("FetchFor() err = %v, want ErrNotFound", err)
	}
}

func TestRecordFetchFailure_KeepsTheReasonAndTheRetryTime(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	retryAt := now.Add(15 * time.Minute)
	if err := store.RecordFetchFailure(broadcastID, "fragment download failed", now, retryAt); err != nil {
		t.Fatalf("RecordFetchFailure() err = %v, want nil", err)
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.LastError != "fragment download failed" {
		t.Errorf("LastError = %q, want the reason recorded", fetch.LastError)
	}
	if !fetch.NextAttemptAt.Equal(retryAt) {
		t.Errorf("NextAttemptAt = %v, want %v", fetch.NextAttemptAt, retryAt)
	}
	if fetch.State != FetchPending {
		t.Errorf("State = %q, want %q for a retryable failure", fetch.State, FetchPending)
	}
}

func TestRefuseFetch_MarksABroadcastNothingMayFetch(t *testing.T) {
	// The operator deleting every copy of a broadcast is a decision about
	// that broadcast, and a recovery pass that downloads it back inside the
	// hour spends the space the deletion freed.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if err := store.RefuseFetch(broadcastID, "the operator purged it", now); err != nil {
		t.Fatalf("RefuseFetch() err = %v, want nil", err)
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.State != FetchTerminal {
		t.Errorf("State = %q, want %q", fetch.State, FetchTerminal)
	}
	if fetch.LastError != "the operator purged it" {
		t.Errorf("LastError = %q, want the stated reason", fetch.LastError)
	}
	if !fetch.NextAttemptAt.IsZero() {
		t.Errorf("NextAttemptAt = %v, want none: no timer may move a refusal", fetch.NextAttemptAt)
	}

	claimed, err := store.ClaimFetch(broadcastID, 1, now.Add(time.Hour), fetchLease)
	if err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if claimed {
		t.Error("ClaimFetch() = true for a refused broadcast, want false")
	}
}

func TestRefuseFetch_OverridesAnEarlierAttempt(t *testing.T) {
	// A broadcast fetched once and then purged has a row already, and the
	// refusal has to reach that row rather than being dropped as a
	// conflict.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if err := store.ReleaseFetch(broadcastID, FetchDone, now); err != nil {
		t.Fatalf("ReleaseFetch() err = %v, want nil", err)
	}

	if err := store.RefuseFetch(broadcastID, "the operator purged it", now.Add(time.Hour)); err != nil {
		t.Fatalf("RefuseFetch() err = %v, want nil", err)
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.State != FetchTerminal {
		t.Errorf("State = %q, want %q", fetch.State, FetchTerminal)
	}
}

func TestFetchState_Valid(t *testing.T) {
	// A state nothing recognises is claimable, because a claim excludes the
	// two it names rather than admitting the two it wants, and due() treats
	// it as ready. An unknown value is not an inert typo: it is a broadcast
	// fetched on every pass forever.
	tests := []struct {
		state FetchState
		want  bool
	}{
		{state: FetchPending, want: true},
		{state: FetchClaimed, want: true},
		{state: FetchDone, want: true},
		{state: FetchTerminal, want: true},
		{state: FetchState(""), want: false},
		{state: FetchState("finished"), want: false},
		{state: FetchState("'; DROP TABLE broadcasts; --"), want: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.Valid(); got != tt.want {
				t.Errorf("FetchState(%q).Valid() = %t, want %t", tt.state, got, tt.want)
			}
		})
	}
}

func TestReleaseFetch_LeavesARefusedBroadcastRefused(t *testing.T) {
	// The operator can refuse a broadcast while a fetch of it is already
	// running. Terminal is chosen precisely because nothing moves a row out
	// of it, so the fetch finishing afterwards must not undo the refusal
	// and have the next pass download the broadcast back.
	store := newStore(t)
	channel := newChannel(t, store)
	broadcastID := newBroadcastFor(t, store, channel.ID, broadcastStart)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimFetch(broadcastID, 1, at, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("ClaimFetch() = %t, %v, want it claimed", claimed, err)
	}
	if err := store.RefuseFetch(broadcastID, "the operator purged every copy", at); err != nil {
		t.Fatalf("RefuseFetch() err = %v, want nil", err)
	}

	// The in-flight fetch finishing.
	if err := store.ReleaseFetch(broadcastID, FetchPending, at.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReleaseFetch() err = %v against a refused row, want ErrNotFound", err)
	}
	if err := store.RecordFetchFailure(broadcastID, "a transient failure",
		at.Add(time.Minute), at.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordFetchFailure() err = %v against a refused row, want ErrNotFound", err)
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.State != FetchTerminal {
		t.Errorf("state = %q, want the refusal to stand as %q", fetch.State, FetchTerminal)
	}
}

func TestReleaseFetch_RefusesAStateNothingRecognises(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	broadcastID := newBroadcastFor(t, store, channel.ID, broadcastStart.Add(time.Hour))

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if _, err := store.ClaimFetch(broadcastID, 1, at, time.Hour); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}

	if err := store.ReleaseFetch(broadcastID, FetchState("finished"), at); err == nil {
		t.Error("ReleaseFetch() accepted a state nothing recognises, want a refusal")
	}
}

// ///////////////////////////////////////////////
// AbandonFetch
// ///////////////////////////////////////////////

func TestAbandonFetch_GivesTheBroadcastBackExactlyAsItWasFound(t *testing.T) {
	// A capture beginning cancels the round around it, and the fetch it
	// interrupted downloaded nothing and learned nothing. Left as an
	// ordinary release it would hold the broadcast for the rest of the
	// lease, and the attempt the claim counted would stand.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 42, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if err := store.AbandonFetch(broadcastID, 42, now.Add(time.Minute)); err != nil {
		t.Fatalf("AbandonFetch() err = %v, want nil", err)
	}

	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.Attempts != 0 {
		t.Errorf("Attempts = %d, want the untried attempt given back", fetch.Attempts)
	}
	if fetch.State != FetchPending {
		t.Errorf("State = %q, want %q", fetch.State, FetchPending)
	}

	// The whole point: the next round can take it straight away rather than
	// waiting out a six-hour lease.
	retaken, err := store.ClaimFetch(broadcastID, 43, now.Add(2*time.Minute), fetchLease)
	if err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if !retaken {
		t.Error("ClaimFetch() = false straight after an abandon, want the broadcast free")
	}
}

func TestAbandonFetch_LeavesATerminalBroadcastAlone(t *testing.T) {
	// The operator can refuse a broadcast while a fetch of it is running.
	// A round cancelled afterwards must not return it to pending.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 1, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}
	if err := store.RecordFetchFailure(broadcastID, "video is private", now, time.Time{}); err != nil {
		t.Fatalf("RecordFetchFailure() err = %v, want nil", err)
	}

	if err := store.AbandonFetch(broadcastID, 42, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("AbandonFetch() err = %v against a terminal row, want ErrNotFound", err)
	}
	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.State != FetchTerminal {
		t.Errorf("State = %q, want it left %q", fetch.State, FetchTerminal)
	}
}

func TestAbandonFetch_UndoesOnlyTheClaimItsCallerStillHolds(t *testing.T) {
	// A fetch cancelled after it recorded an outcome has already moved the
	// row out of claimed. Marking a broadcast done, and charging a failure
	// against it, are both answers this must not rewind: undoing a done row
	// has the next round download a broadcast already in the library.
	tests := []struct {
		name   string
		settle func(t *testing.T, store *Store, broadcastID int64, at time.Time)
		want   FetchState
	}{
		{
			name: "already in the library",
			settle: func(t *testing.T, store *Store, broadcastID int64, at time.Time) {
				if err := store.ReleaseFetch(broadcastID, FetchDone, at); err != nil {
					t.Fatalf("ReleaseFetch() err = %v, want nil", err)
				}
			},
			want: FetchDone,
		},
		{
			name: "a failure already charged",
			settle: func(t *testing.T, store *Store, broadcastID int64, at time.Time) {
				if err := store.RecordFetchFailure(broadcastID, "the fragment failed",
					at, at.Add(time.Hour)); err != nil {
					t.Fatalf("RecordFetchFailure() err = %v, want nil", err)
				}
			},
			want: FetchPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)
			now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
			broadcastID := newBroadcastFor(t, store, channel.ID, now)

			if _, err := store.ClaimFetch(broadcastID, 42, now, fetchLease); err != nil {
				t.Fatalf("ClaimFetch() err = %v, want nil", err)
			}
			tt.settle(t, store, broadcastID, now.Add(time.Minute))

			if err := store.AbandonFetch(broadcastID, 42, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Errorf("AbandonFetch() err = %v after an outcome was recorded, want ErrNotFound", err)
			}
			fetch, err := store.FetchFor(broadcastID)
			if err != nil {
				t.Fatalf("FetchFor() err = %v, want nil", err)
			}
			if fetch.State != tt.want {
				t.Errorf("State = %q, want it left %q", fetch.State, tt.want)
			}
			if fetch.Attempts != 1 {
				t.Errorf("Attempts = %d, want the recorded try left standing", fetch.Attempts)
			}
		})
	}
}

func TestAbandonFetch_RefusesAClaimHeldByAnotherSession(t *testing.T) {
	// A lease that expired and was taken over by another recorder. The
	// first one finishing afterwards must not hand back work in progress.
	store := newStore(t)
	channel := newChannel(t, store)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	broadcastID := newBroadcastFor(t, store, channel.ID, now)

	if _, err := store.ClaimFetch(broadcastID, 7, now, fetchLease); err != nil {
		t.Fatalf("ClaimFetch() err = %v, want nil", err)
	}

	if err := store.AbandonFetch(broadcastID, 8, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("AbandonFetch() err = %v for another session's claim, want ErrNotFound", err)
	}
	fetch, err := store.FetchFor(broadcastID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want nil", err)
	}
	if fetch.ClaimedBy != 7 {
		t.Errorf("ClaimedBy = %d, want the original holder 7", fetch.ClaimedBy)
	}
}
