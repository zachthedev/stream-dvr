package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// FetchState is how far backfill has got with one broadcast.
type FetchState string

// Fetch is what backfill remembers about one broadcast.
//
// It lives across restarts on purpose. Backoff held in memory resets every
// time the daemon starts, and a recorder restarts on upgrades and reboots,
// so an in-memory retry schedule turns into a hot loop against someone
// else's service.
type Fetch struct {
	// BroadcastID is the broadcast this describes.
	BroadcastID int64
	// State is how far this has got.
	State FetchState
	// Attempts counts fetches started, successful or not.
	Attempts int
	// NextAttemptAt is when a retry may begin. Zero means now.
	NextAttemptAt time.Time
	// LastError is the most recent failure, for the operator to read.
	LastError string
	// ClaimedAt is when a fetcher took this broadcast. Zero means nobody
	// holds it.
	ClaimedAt time.Time
	// ClaimedBy is the daemon session holding it.
	ClaimedBy int64
	// UpdatedAt is when this row last changed.
	UpdatedAt time.Time
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Fetch states.
const (
	// FetchPending means a broadcast is a candidate nobody has taken.
	FetchPending FetchState = "pending"
	// FetchClaimed means a fetcher is working on it now.
	FetchClaimed FetchState = "claimed"
	// FetchDone means the recording is in the library.
	FetchDone FetchState = "done"
	// FetchTerminal means this will never succeed and must not be
	// retried: the video is private, removed, or needs a login. A timer
	// must never move a broadcast out of this state.
	FetchTerminal FetchState = "terminal"
)

// Valid reports whether a fetch state is one this build knows.
//
// A state nothing recognises is claimable, because a claim excludes the
// two it names rather than admitting the two it wants, and due() treats it
// as ready. So an unknown value is not an inert typo: it is a broadcast
// fetched on every pass forever.
func (f FetchState) Valid() bool {
	switch f {
	case FetchPending, FetchClaimed, FetchDone, FetchTerminal:
		return true
	default:
		return false
	}
}

// ///////////////////////////////////////////////
// Claims
// ///////////////////////////////////////////////

// ClaimFetch takes a broadcast for one fetcher, reporting whether it got it.
//
// The claim is the whole statement, so two fetchers racing cannot both win:
// the conflicting upsert only writes where the row is unclaimed or its lease
// has run out, and a write that does not happen returns no row.
//
// A lease rather than a flag, because a fetcher that crashes mid-download
// cannot release what it holds. Without an expiry that broadcast would be
// unfetchable for as long as the library lives.
//
// A terminal or done row is never claimed. That is what keeps a private
// video from being retried on every pass forever, and a broadcast already in
// the library from being downloaded a second time.
func (s *Store) ClaimFetch(broadcastID, claimedBy int64, at time.Time, lease time.Duration) (bool, error) {
	if lease <= 0 {
		return false, fmt.Errorf("fetch lease %s must be positive", lease)
	}
	if err := requireStorable(at); err != nil {
		return false, fmt.Errorf("claiming broadcast %d for fetch: %w", broadcastID, err)
	}

	var claimed int64
	err := s.db.QueryRow(`
		INSERT INTO broadcast_fetches
			(broadcast_id, state, attempts, next_attempt_at, last_error, claimed_at, claimed_by, updated_at)
		VALUES (?, ?, 1, NULL, '', ?, ?, ?)
		ON CONFLICT (broadcast_id) DO UPDATE SET
			state      = ?,
			attempts   = broadcast_fetches.attempts + 1,
			claimed_at = excluded.claimed_at,
			claimed_by = excluded.claimed_by,
			updated_at = excluded.updated_at
		WHERE broadcast_fetches.state <> ?
		  AND broadcast_fetches.state <> ?
		  AND (broadcast_fetches.claimed_at IS NULL OR broadcast_fetches.claimed_at <= ?)
		  AND (broadcast_fetches.next_attempt_at IS NULL OR broadcast_fetches.next_attempt_at <= ?)
		RETURNING broadcast_id`,
		broadcastID, FetchClaimed, encodeTime(at), claimedBy, encodeTime(at),
		FetchClaimed, FetchTerminal, FetchDone, encodeTime(at.Add(-lease)), encodeTime(at),
	).Scan(&claimed)

	if errors.Is(err, sql.ErrNoRows) {
		// Somebody else holds it, it is terminal or done, or its backoff has
		// not elapsed. None of those is a failure worth reporting upward.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claiming broadcast %d for fetch: %w", broadcastID, err)
	}
	return true, nil
}

// ReleaseFetch records that a fetch finished, and drops the claim.
//
// The claim is cleared whatever the state, so a broadcast is never left
// held by a fetcher that has moved on.
//
// A terminal row is left alone, and the caller learns that through
// ErrNotFound. The operator can refuse a broadcast while a fetch of it is
// already running, and the fetch finishing afterwards must not undo that:
// terminal is chosen precisely because nothing moves a row out of it.
func (s *Store) ReleaseFetch(broadcastID int64, state FetchState, at time.Time) error {
	if !state.Valid() {
		return fmt.Errorf("fetch state %q is not valid", state)
	}
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("releasing broadcast %d: %w", broadcastID, err)
	}

	result, err := s.db.Exec(`
		UPDATE broadcast_fetches
		SET state = ?, claimed_at = NULL, claimed_by = NULL, last_error = '', updated_at = ?
		WHERE broadcast_id = ? AND state <> ?`,
		state, encodeTime(at), broadcastID, FetchTerminal)
	if err != nil {
		return fmt.Errorf("releasing broadcast %d: %w", broadcastID, err)
	}
	return requireRow(result, broadcastID, "fetch for broadcast")
}

// RecordFetchFailure stores why a fetch failed and when to try again.
//
// A zero retryAt means never: the row goes terminal and no timer moves it
// out again. That is the answer for a video that is private, removed, or
// needs a login, where retrying is asking the same question forever.
//
// A row already terminal is left alone, so a fetch failing after the
// operator refused the broadcast cannot return it to pending.
func (s *Store) RecordFetchFailure(broadcastID int64, reason string, at, retryAt time.Time) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("recording a fetch failure for broadcast %d: %w", broadcastID, err)
	}

	state := FetchPending
	if retryAt.IsZero() {
		state = FetchTerminal
	}

	result, err := s.db.Exec(`
		UPDATE broadcast_fetches
		SET state = ?, last_error = ?, next_attempt_at = ?,
		    claimed_at = NULL, claimed_by = NULL, updated_at = ?
		WHERE broadcast_id = ? AND state <> ?`,
		state, reason, encodeTimePtr(nilIfZero(retryAt)), encodeTime(at), broadcastID, FetchTerminal)
	if err != nil {
		return fmt.Errorf("recording a fetch failure for broadcast %d: %w", broadcastID, err)
	}
	return requireRow(result, broadcastID, "fetch for broadcast")
}

// AbandonFetch gives a broadcast back exactly as it was found, for a claim
// that ended without trying anything.
//
// A capture beginning cancels the round around it, and the fetch it
// interrupted downloaded nothing and learned nothing. Releasing it as an
// ordinary failure would hold it for the rest of its lease and leave the
// attempt the claim counted standing, and the retry delay grows with that
// count: a handful of interruptions would push a broadcast nobody ever
// tried to a day between tries.
//
// A row this caller no longer holds answers ErrNotFound, which is how a
// caller learns there was nothing of its own left to undo.
func (s *Store) AbandonFetch(broadcastID, claimedBy int64, at time.Time) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("abandoning the fetch of broadcast %d: %w", broadcastID, err)
	}

	// Scoped to the claim this caller still holds, not merely to a row that
	// is not terminal. A fetch cancelled after it recorded an outcome has
	// already moved the row out of claimed: marking the broadcast done, or
	// charging a failure against it, are both answers this must not rewind.
	// Undoing a done row is the expensive one, because the next round then
	// downloads a broadcast that is already in the library.
	//
	// Floored at zero, so a release with no matching claim behind it, which
	// only a hand-edited row produces, cannot drive the count negative.
	result, err := s.db.Exec(`
		UPDATE broadcast_fetches
		SET state = ?, attempts = max(attempts - 1, 0),
		    claimed_at = NULL, claimed_by = NULL, updated_at = ?
		WHERE broadcast_id = ? AND state = ? AND claimed_by = ?`,
		FetchPending, encodeTime(at), broadcastID, FetchClaimed, claimedBy)
	if err != nil {
		return fmt.Errorf("abandoning the fetch of broadcast %d: %w", broadcastID, err)
	}
	return requireRow(result, broadcastID, "fetch for broadcast")
}

// RefuseFetch marks a broadcast nothing may fetch, with the reason stated.
//
// It is how a decision made outside backfill reaches it. Deleting every copy
// of a broadcast says the operator does not want that broadcast, and without
// this the recovery pass reads the empty day as a broadcast it missed and
// downloads it back.
//
// Terminal is the right shape for that: no timer moves a row out of it, and
// the claim already refuses one. The reason is what an operator reading the
// row sees, so it must name who refused rather than only that something did.
func (s *Store) RefuseFetch(broadcastID int64, reason string, at time.Time) error {
	if reason == "" {
		return fmt.Errorf("refusing to fetch broadcast %d needs a reason", broadcastID)
	}
	if err := requireStorable(at); err != nil {
		return err
	}

	// Upserted rather than updated: a broadcast nobody ever fetched has no
	// row at all, which is the ordinary case for one the operator purged
	// straight after the recorder captured it.
	_, err := s.db.Exec(`
		INSERT INTO broadcast_fetches
			(broadcast_id, state, attempts, next_attempt_at, last_error, claimed_at, claimed_by, updated_at)
		VALUES (?, ?, 0, NULL, ?, NULL, NULL, ?)
		ON CONFLICT (broadcast_id) DO UPDATE SET
			state           = excluded.state,
			next_attempt_at = NULL,
			last_error      = excluded.last_error,
			claimed_at      = NULL,
			claimed_by      = NULL,
			updated_at      = excluded.updated_at`,
		broadcastID, FetchTerminal, reason, encodeTime(at))
	if err != nil {
		return fmt.Errorf("refusing to fetch broadcast %d: %w", broadcastID, err)
	}
	return nil
}

// FetchFor returns what is remembered about a broadcast's fetches.
//
// A broadcast nobody has tried has no row, which reports as ErrNotFound
// rather than as a zero Fetch: "never attempted" and "attempted, no state"
// are different answers and a caller acts differently on each.
func (s *Store) FetchFor(broadcastID int64) (Fetch, error) {
	var (
		fetch                             Fetch
		nextAttempt, claimedAt, claimedBy sql.NullInt64
		updatedAt                         int64
	)

	err := s.db.QueryRow(`
		SELECT broadcast_id, state, attempts, next_attempt_at, last_error, claimed_at, claimed_by, updated_at
		FROM broadcast_fetches WHERE broadcast_id = ?`, broadcastID).
		Scan(&fetch.BroadcastID, &fetch.State, &fetch.Attempts, &nextAttempt,
			&fetch.LastError, &claimedAt, &claimedBy, &updatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Fetch{}, fmt.Errorf("fetch for broadcast %d: %w", broadcastID, ErrNotFound)
	}
	if err != nil {
		return Fetch{}, fmt.Errorf("reading the fetch for broadcast %d: %w", broadcastID, err)
	}

	if nextAttempt.Valid {
		fetch.NextAttemptAt = decodeTime(nextAttempt.Int64)
	}
	if claimedAt.Valid {
		fetch.ClaimedAt = decodeTime(claimedAt.Int64)
	}
	fetch.ClaimedBy = claimedBy.Int64
	fetch.UpdatedAt = decodeTime(updatedAt)
	return fetch, nil
}

// nilIfZero turns a zero time into the absent value the column stores.
func nilIfZero(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
