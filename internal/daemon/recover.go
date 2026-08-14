package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Round describes one recovery round the recorder asked for.
type Round struct {
	// Since is the earliest instant the round must cover.
	Since time.Time
	// Session is the claim this process holds. A fetch records it, so a
	// crash mid-download leaves a claim that expires with the session
	// rather than one nothing will ever release.
	Session int64
	// Platforms names the platforms that answered a probe, and a round
	// covers only channels on one of them. Fetching against a service that
	// is down achieves nothing and pushes every broadcast on it further
	// into its backoff, so those channels wait for the round that runs once
	// their platform answers.
	Platforms []string
	// Report carries one thing the round did to the operator.
	//
	// Only an outcome that does not resolve on its own reaches a sink this
	// way. A round after a long outage recovers broadcasts by the dozen,
	// and one notification each is a burst nobody reads: those are counted
	// and told in a single summary once the round ends.
	Report func(Event)
}

// RoundResult is what one recovery round achieved.
type RoundResult struct {
	// Recovered counts the broadcasts fetched whole.
	Recovered int
	// Failed counts what went wrong and might not go wrong next time. A
	// round reports its failures per channel and carries on, so without
	// this a round where every listing timed out is indistinguishable from
	// one that found the library already complete, and the window it was
	// covering would be dropped as done.
	Failed int
	// GapsFilled counts the holes patched inside captured broadcasts.
	GapsFilled int
	// Deferred counts the broadcasts something else was holding, so this
	// round could not try them. Nothing is wrong and the work is still
	// outstanding, which is a different thing from a round with nothing
	// left to do.
	Deferred int
}

// probe is what the last poll of one channel established.
type probe struct {
	// platform is the channel's platform, so reachability is judged for the
	// service that answered rather than for the recorder as a whole.
	platform string
	// answered says whether that platform replied.
	answered bool
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// recoveryInterval is how often a round runs with nothing having gone
// wrong.
//
// Slow, because what it finds is what every other mechanism already missed:
// a broadcast shorter than one poll, a capture that failed, a hole inside a
// capture that only becomes fillable once the broadcast settles. None of
// those appear between one hour and the next, and every round is a listing
// request against somebody else's service.
const recoveryInterval = 6 * time.Hour

// recoveryFloor is the least time between two rounds.
//
// Without it a link that drops and returns every few minutes drives a round
// each time it comes back, and each round is a listing request per channel.
// This paces the requests and nothing more. What stops a bad afternoon
// costing anything lasting is that a failed fetch waits longer each time and
// only a classified answer from the platform retires a broadcast, both of
// which the fetcher decides.
const recoveryFloor = 30 * time.Minute

// outageFloor is how long the recorder has to have been unable to see a
// channel before anything could have been missed.
//
// Below it there is nothing to recover: a broadcast that started and ended
// inside a gap this short is not a broadcast. It also keeps a restart, a
// laptop lid, or one refused probe from queueing a round that would list
// every channel and find exactly nothing.
const outageFloor = 5 * time.Minute

// recoveryMargin extends a round back past the outage it is recovering.
//
// A broadcast that began before the outage and ran into it is missed from
// the moment the recorder stopped seeing it, and a window that starts when
// the outage started excludes the row entirely: a broadcast is in range by
// where it began, not by which hours of it are missing.
const recoveryMargin = 12 * time.Hour

// recoveryHorizon is how far back automatic recovery reaches.
//
// It bounds a round asked to cover more, and it is also what the routine
// round asks for outright. Both readings matter. A machine that lost power
// and came back a month later reports a month of downtime, and a round that
// took that literally would list an entire archive and download it,
// unattended, onto a volume sized for a fortnight. Meanwhile a round that
// was cancelled or failed queues its window again, and the routine round
// has to reach the whole way back for that window to return if the process
// died in between.
//
// Twitch keeps an ordinary channel's copies for fourteen days, so past this
// there is usually nothing left upstream to fetch anyway. Reaching further
// is deliberate work with a number the operator chose, which is what the
// backfill command is for.
const recoveryHorizon = 14 * 24 * time.Hour

// roundDeadline bounds one round.
//
// The loop is one goroutine on purpose, so a download that stalls without
// ever failing takes recovery down with it and nothing says so: the hold
// report only covers a round that never started. Comfortably inside the
// routine cadence, so a round that hits this is over before the next one is
// due.
const roundDeadline = 4 * time.Hour

// holdReportAfter is how long a round may sit held before the operator is
// told outright.
//
// Every condition that holds a round is meant to clear on its own: a
// capture ends, the pacing floor passes, a platform answers. One that has
// not cleared in this long is a recorder quietly recovering nothing, and a
// channel misspelled in the config puts it there permanently.
const holdReportAfter = 12 * time.Hour

// errNothingToCover reports a window with nothing in it to recover, as
// against one that could not be worked out. The first is an answer and the
// second is a failure, and only the second is worth asking again.
var errNothingToCover = errors.New("nothing in this window could have been missed")

// ///////////////////////////////////////////////
// Scheduling
// ///////////////////////////////////////////////

// recoveryLoop runs recovery rounds until ctx is cancelled.
//
// One goroutine, so rounds are serialized by construction rather than by a
// flag two of them could race on. A round can run for hours, and a second
// one starting alongside it would fetch the same broadcasts.
func (d *Daemon) recoveryLoop(ctx context.Context) {
	if d.recovery == nil {
		return
	}

	ticker := time.NewTicker(d.recoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Not a measured gap: this asks for as far back as recovery
			// may reach, so meeting the horizon is the request being
			// honoured rather than a window being trimmed.
			d.requestRecovery(d.now().Add(-recoveryHorizon), "the routine round", time.Time{})
		case <-d.recoveryWake:
		}
		d.runRecovery(ctx)
	}
}

// requestRecovery asks for a round covering everything since from.
//
// missingSince is the instant a measured gap stopped covering, or the zero
// time where nothing measured one.
//
// Requests coalesce: several channels coming back from one outage, or an
// outage that ends while a round is already queued, produce one round
// reaching back as far as the earliest of them asked for.
func (d *Daemon) requestRecovery(from time.Time, why string, missingSince time.Time) {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	d.queueRecovery(from, why, missingSince)
}

// queueRecovery records a pending round. The caller holds recoveryMu.
func (d *Daemon) queueRecovery(from time.Time, why string, missingSince time.Time) {
	// Kept whichever request wins below, and reaching back to the earliest
	// of them. A measured gap discarded along with its window is a round
	// that trims real coverage and says nothing, and the request that wins
	// is routinely the one nobody measured: the routine round asks for the
	// whole horizon, which reaches further back than most outages do.
	// The reason travels with the measurement rather than with the window,
	// because the warning is about the measurement. The two coalesce
	// separately, so pairing the surviving window's reason with the
	// surviving measurement would credit a thirteen-day outage to the
	// six-hourly tick that happened to ask for a wider window.
	if !missingSince.IsZero() &&
		(d.recoveryMissingSince.IsZero() || missingSince.Before(d.recoveryMissingSince)) {
		d.recoveryMissingSince, d.recoveryMissingWhy = missingSince, why
	}

	// A round already reaching further back covers this one, so the reason
	// travelling with it stays the reason for the earliest window.
	if d.recoveryWhy != "" && !from.Before(d.recoveryFrom) {
		return
	}
	d.recoveryFrom, d.recoveryWhy = from, why
	d.wakeRecovery()
}

// wakeRecovery pokes the loop. The caller holds recoveryMu.
//
// The channel holds one poke and the send never blocks, because the loop
// reads the request rather than the poke: a second one while a round is
// already queued would only make the loop check a question it has already
// been asked.
func (d *Daemon) wakeRecovery() {
	select {
	case d.recoveryWake <- struct{}{}:
	default:
	}
}

// noteProbe records whether a channel's platform answered.
//
// It is the daemon's only evidence about reachability, and it is taken from
// the probe rather than from the whole cycle: a cycle lasts as long as the
// broadcast it captures, so a channel that went live an hour ago would
// otherwise have reported nothing since.
//
// An outage that ends asks for a round covering it. A probe that answers
// while a round is already queued pokes the loop, because a round held for
// want of a reachable platform has no other event to release it.
func (d *Daemon) noteProbe(channelID int64, platform string, answered bool) {
	at := d.now()

	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()

	d.probed[channelID] = probe{platform: platform, answered: answered}
	if !answered {
		// Only the first failure of a run dates the outage. Overwriting on
		// every failed poll would date it to the last one, and an outage
		// measured from its own end is always zero.
		if _, ongoing := d.outages[channelID]; !ongoing {
			d.outages[channelID] = at
		}
		return
	}

	if began, ongoing := d.outages[channelID]; ongoing {
		delete(d.outages, channelID)
		if down := at.Sub(began); down >= outageFloor {
			d.queueRecovery(began.Add(-recoveryMargin),
				fmt.Sprintf("the platform was unreachable for %s", roundDuration(down)), began)
		}
	}
	if d.recoveryWhy != "" {
		d.wakeRecovery()
	}
}

// ///////////////////////////////////////////////
// Rounds
// ///////////////////////////////////////////////

// runRecovery runs the pending round, if there is one and nothing is
// holding it.
func (d *Daemon) runRecovery(ctx context.Context) {
	if !d.recoveryPending() {
		return
	}
	// Read once and carried through. Judging the gate on one snapshot and
	// handing the round another lets a single dropped poll in between admit
	// a round and then give it nothing to cover.
	platforms := d.reachablePlatforms()
	if held := d.recoveryHeldBy(platforms); held != "" {
		d.reportHold(ctx, held)
		return
	}
	d.clearHold()

	from, why, missingSince, pending := d.takeRecovery()
	if !pending {
		return
	}

	bounded, boundErr := d.boundRound(ctx, from, why, missingSince)
	switch {
	case errors.Is(boundErr, errNothingToCover):
		return
	case boundErr != nil:
		// The window could not be bounded, and an unbounded one downloads an
		// archive. Queued again rather than dropped: a database busy for the
		// moment it was asked must not cost a downtime window a restart has
		// just measured.
		// Paced as though a round had run. The requeue below wakes the loop,
		// and a wake this path did not pace would be drained, fail to bound
		// again, and wake itself: a database that stays unreadable would spin
		// the loop at full speed and overwrite the whole retained log.
		d.noteRoundRan(false)
		d.requestRecovery(from, why, missingSince)
		d.logger.ErrorContext(ctx, "could not bound a recovery round, so its window is queued again",
			slog.Any("error", boundErr))
		return
	}
	from = bounded

	d.logger.InfoContext(ctx, "recovering what was missed",
		slog.String("because", why),
		slog.Duration("reaching_back", roundDuration(d.now().Sub(from))))

	// The round gets a context a capture can cancel, and a deadline of its
	// own. It runs for hours, and the check that held it off only ran once:
	// a broadcast starting an hour in would otherwise leave a download
	// competing with the recording for the disk and the link, and the
	// watermark ends the capture while nothing at all ends an admitted
	// download. The deadline covers the other way this goes wrong: the loop
	// is one goroutine on purpose, so a download that stalls without
	// failing would stop recovery for good and say nothing.
	round, stop := context.WithTimeout(ctx, roundDeadline)
	d.holdRoundCancel(stop)
	// A capture that began between the hold check above and this install
	// found no cancel to call, so it could not stop the round it should
	// have. Asking once more now that the cancel is reachable closes that
	// window: a capture is either seen here or late enough to find it.
	if d.anyCapturing() {
		stop()
	}
	result, err := d.recovery(round, Round{
		Since:     from,
		Session:   d.currentSession(),
		Platforms: platforms,
		Report:    func(event Event) { d.reportOutcome(ctx, event) },
	})
	d.holdRoundCancel(nil)
	stop()

	switch {
	case ctx.Err() != nil:
		// Queued again rather than dropped, so a shutdown that lands mid
		// round does not also consume the window. It is held in memory
		// only: if the process does not come back, the routine round's
		// reach is what brings the window back.
		d.noteRoundRan(true)
		d.requestRecovery(from, why, missingSince)
		d.logger.InfoContext(ctx, "the recovery round stopped early because the recorder is shutting down",
			slog.Int("recovered", result.Recovered))
	case errors.Is(err, context.DeadlineExceeded):
		d.noteRoundRan(false)
		d.requestRecovery(from, why, missingSince)
		d.logger.ErrorContext(ctx, "the recovery round ran out of time and was stopped",
			slog.Duration("after", roundDeadline), slog.Int("recovered", result.Recovered))
		d.notify(ctx, Event{
			Kind: EventFailure,
			Detail: fmt.Sprintf("a recovery round was still running after %s and was stopped, "+
				"so a download is probably stalled", roundDuration(roundDeadline)),
		})
	case errors.Is(err, context.Canceled):
		// A capture took the disk and the link back. Designed behaviour, so
		// it is neither reported as a failure nor paced as one: on a channel
		// that streams most days every round ends this way, and charging
		// each one would push the cadence to its ceiling and leave it there.
		d.noteRoundRan(true)
		d.requestRecovery(from, why, missingSince)
		d.logger.InfoContext(ctx, "the recovery round stood aside for a capture",
			slog.Int("recovered", result.Recovered))
	case err != nil:
		d.noteRoundRan(false)
		d.requestRecovery(from, why, missingSince)
		d.logger.ErrorContext(ctx, "the recovery round failed, so its window is queued again",
			slog.Any("error", err))
	case result.Failed > 0:
		d.noteRoundRan(false)
		d.requestRecovery(from, why, missingSince)
		d.logger.WarnContext(ctx, "part of the recovery round did not work, so its window is queued again",
			slog.Int("recovered", result.Recovered), slog.Int("failed", result.Failed))
	case result.Deferred > 0:
		// Nothing went wrong and nothing got done: everything the round
		// wanted was held by something else, or sat on a platform that is
		// not answering. The window stays queued rather than being consumed
		// by a round that fetched none of it, and the cadence backs off,
		// because asking again sooner would not change the answer.
		d.noteRoundRan(false)
		d.requestRecovery(from, why, missingSince)
		d.logger.InfoContext(ctx, "the recovery round reached nothing it could take yet",
			slog.Int("waiting_on", result.Deferred))
	default:
		d.noteRoundRan(true)
		d.logger.InfoContext(ctx, "the recovery round finished",
			slog.Int("recovered", result.Recovered))
	}
	// Detached from the run's own context, so a round that recovered
	// something before a shutdown interrupted it still says so. Both
	// desktop sinks honour the caller's context, and this is the only
	// account the operator gets now that outcomes are summarized.
	d.summarizeRound(context.WithoutCancel(ctx), result)
}

// boundRound clamps a window to what automatic recovery may cover, and
// reports whether anything is left of it.
//
// Two bounds, for two different mistakes. The horizon stops a month of
// downtime becoming a month of unattended downloads. The library's first
// session stops a fresh install treating a channel's entire archive as
// something it missed: a recorder cannot have missed what aired before it
// existed, and the routine round asks for the whole horizon outright.
func (d *Daemon) boundRound(ctx context.Context, from time.Time, why string, missingSince time.Time) (time.Time, error) {
	now := d.now()

	first, err := d.store.FirstSessionStart()
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No recorder has ever held this library, so nothing was missed.
		return time.Time{}, errNothingToCover
	case err != nil:
		return time.Time{}, fmt.Errorf("reading when this library was first recorded to: %w", err)
	}

	// Read before either bound is applied, because it bounds the loss as
	// well as the window. A recorder cannot have missed what aired before
	// it existed, so a measured start earlier than the library is a clock
	// or a restored row rather than coverage anybody lost, and reporting it
	// sends the operator fetching a stretch that provably holds nothing.
	if !missingSince.IsZero() && missingSince.Before(first) {
		missingSince = first
	}
	if first.After(from) {
		from = first
	}

	if now.Sub(from) > recoveryHorizon {
		reach := now.Add(-recoveryHorizon)

		// One question, asked of the two instants that answer it: does the
		// round now start after coverage stopped. Nothing else has to be
		// reasoned about, because neither instant carries the margin the
		// request is padded with or the clamp the request already met.
		//
		// The routine round measured nothing, so it loses nothing by
		// meeting the horizon it asked for outright, and says so by leaving
		// this zero. A gap that fits inside the horizon loses nothing
		// either until a long hold pushes the clamp past where it began.
		//
		// Below the floor there was nothing to miss, and saying so every
		// time a round is requeued would repeat the whole line, with the
		// figure grown by the interval, for as long as recovery kept
		// failing.
		if lost := reach.Sub(missingSince); !missingSince.IsZero() && lost >= outageFloor {
			d.logger.WarnContext(ctx, "the gap reaches further back than automatic recovery does, "+
				"so only its most recent part is fetched",
				slog.String("because", why),
				slog.Time("missing_since", missingSince),
				slog.Duration("recovering", recoveryHorizon),
				slog.Duration("trimmed", roundDuration(lost)),
				slog.String("to_reach_further", "stream-dvr backfill --since"))
		}
		from = reach
	}

	if !from.Before(now) {
		// A window ending before it starts covers nothing. It is what a
		// clock stepping backwards leaves behind, and what a library first
		// claimed moments ago leaves too.
		return time.Time{}, errNothingToCover
	}
	return from, nil
}

// reportOutcome tells the operator about one thing a round did, when it is
// the kind that needs telling on its own.
//
// A broadcast the round gave up on is the only one. It does not resolve
// without the operator, where a recovered broadcast and a filled hole are
// both work that finished, and a round after a long outage produces those
// by the dozen.
func (d *Daemon) reportOutcome(ctx context.Context, event Event) {
	switch event.Kind {
	case EventFetchGaveUp:
		d.notify(ctx, event)
	case EventRecovered, EventGapFilled:
		// Counted into the round's summary instead.
	default:
		// A kind nobody wired up here reaches no sink at all. The outcomes
		// are defined in another package and cross as plain strings, so a
		// new one would otherwise vanish without trace.
		d.logger.DebugContext(ctx, "a recovery outcome reached no sink",
			slog.String("kind", string(event.Kind)))
	}
}

// summarizeRound tells the operator what a round achieved, in one event.
//
// A round that recovered nothing says nothing: the operator asked to hear
// about recordings arriving, and a routine round that found the library
// complete is not one.
func (d *Daemon) summarizeRound(ctx context.Context, result RoundResult) {
	if result.Recovered == 0 && result.GapsFilled == 0 {
		return
	}

	detail := fmt.Sprintf("recovered %d %s", result.Recovered,
		plural(result.Recovered, "broadcast", "broadcasts"))
	if result.GapsFilled > 0 {
		detail += fmt.Sprintf(" and filled %d %s", result.GapsFilled,
			plural(result.GapsFilled, "gap", "gaps"))
	}

	d.notify(ctx, Event{Kind: EventRecovered, Detail: detail})
}

// plural picks the word for a count.
func plural(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}

// recoveryHeldBy names what is stopping a round from starting, or "" when
// nothing is.
//
// A capture outranks a round. Both write to the same volume and pull the
// same link, and only the capture is irreplaceable: the recovery packages
// say so themselves, that a recovered copy never displaces a live one. The
// budget alone does not settle it, because the watermark halts the capture
// and nothing halts a download once it has been admitted, so a round
// running alongside a capture can end the recording it was meant to
// complement. A channel that is live has nothing to recover for itself
// anyway, and the round runs as soon as the broadcast ends.
func (d *Daemon) recoveryHeldBy(platforms []string) string {
	if d.anyCapturing() {
		return "a capture is running, and the recording is the copy that cannot be fetched again"
	}
	if pacing := d.roundPacing(); d.roundRecent(pacing) {
		return fmt.Sprintf("the last round finished under %s ago", pacing)
	}
	if len(platforms) == 0 {
		return "no channel has answered a probe, so no platform is known to be reachable"
	}
	return ""
}

// reportHold tells the operator about a round that has been held too long.
//
// Every condition clears on its own in the ordinary case, so the routine
// state is not worth a line. One that has not cleared in holdReportAfter is
// a recorder recovering nothing, which looks from outside exactly like one
// with nothing to recover.
func (d *Daemon) reportHold(ctx context.Context, held string) {
	d.recoveryMu.Lock()
	// Timed from when rounds started being held rather than from the reason
	// they wear now. A busy library alternates between a running capture and
	// the pacing floor, and a timer that restarted on each swap would never
	// reach the reporting threshold on the recorder that most needs it.
	if d.heldSince.IsZero() {
		d.heldSince, d.heldReported = d.now(), d.now()
	}
	d.heldWhy = held
	since, reported := d.heldSince, d.heldReported
	overdue := d.now().Sub(reported) >= holdReportAfter && d.now().Sub(since) >= holdReportAfter
	if overdue {
		d.heldReported = d.now()
	}
	d.recoveryMu.Unlock()

	if !overdue {
		d.logger.DebugContext(ctx, "not starting a recovery round", slog.String("because", held))
		return
	}

	d.logger.WarnContext(ctx, "nothing has been recovered because a round keeps being held",
		slog.String("because", held),
		slog.Duration("held_for", roundDuration(d.now().Sub(since))))
	d.notify(ctx, Event{
		Kind: EventFailure,
		Detail: fmt.Sprintf("recovery has been on hold for %s: %s",
			roundDuration(d.now().Sub(since)), held),
	})
}

// clearHold forgets a hold that has ended, so the next one is timed from
// its own start.
func (d *Daemon) clearHold() {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	d.heldWhy, d.heldSince, d.heldReported = "", time.Time{}, time.Time{}
}

// stopRound cancels a round in flight.
//
// Called when a capture begins, because the check that holds a round off
// runs once and a round outlasts it by hours. The window is queued again,
// so nothing is lost by stopping.
func (d *Daemon) stopRound() {
	d.roundMu.Lock()
	cancel := d.roundCancel
	d.roundMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// holdRoundCancel records how to stop the round now in flight, or clears it
// with nil once there is none.
func (d *Daemon) holdRoundCancel(cancel context.CancelFunc) {
	d.roundMu.Lock()
	defer d.roundMu.Unlock()
	d.roundCancel = cancel
}

// reachablePlatforms names every platform whose channels are answering.
//
// A channel that has never been probed counts for nothing, which is what
// holds the round a restart queues until the first poll confirms the
// machine is back on the network. Judged per platform rather than across
// all of them, because one service being up says nothing about another, and
// a round that fetched against the one that is down would spend its
// broadcasts' attempts on failures.
func (d *Daemon) reachablePlatforms() []string {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()

	var reachable []string
	for _, last := range d.probed {
		if last.answered && !slices.Contains(reachable, last.platform) {
			reachable = append(reachable, last.platform)
		}
	}
	// Sorted so a round covers the same set however the map enumerated.
	slices.Sort(reachable)
	return reachable
}

// roundRecent reports whether the last round finished less than the given
// pacing ago. A library no round has run against yet is never recent.
func (d *Daemon) roundRecent(pacing time.Duration) bool {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	return !d.roundRanAt.IsZero() && d.now().Sub(d.roundRanAt) < pacing
}

// noteRoundRan records that a round has just finished, and paces the next
// one by how this one went.
//
// A round that failed queues its window again, so without this a single
// channel whose listing always fails, which is the misspelled-name case,
// turns the six-hour cadence into a permanent thirty-minute one. Every round
// lists every channel, so that is a standing twelvefold increase in requests
// against somebody else's service, driven by one bad line of config.
func (d *Daemon) noteRoundRan(clean bool) {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()

	d.roundRanAt = d.now()
	if clean {
		d.roundBackoff = recoveryFloor
		return
	}
	// Doubled from the floor rather than towards it, so the first failure
	// already costs something. Landing the first one on the floor exactly
	// would pace a round that failed the same as one that worked.
	d.roundBackoff = min(max(d.roundBackoff*2, 2*recoveryFloor), recoveryInterval)
}

// roundPacing is the least time between this round and the next.
func (d *Daemon) roundPacing() time.Duration {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	return max(d.roundBackoff, recoveryFloor)
}

// recoveryPending reports whether a round is waiting to run.
func (d *Daemon) recoveryPending() bool {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()
	return d.recoveryWhy != ""
}

// takeRecovery removes the pending request and returns it.
func (d *Daemon) takeRecovery() (time.Time, string, time.Time, bool) {
	d.recoveryMu.Lock()
	defer d.recoveryMu.Unlock()

	if d.recoveryWhy == "" {
		// Cleared on the way out as well as on the way in. The measured
		// start is written before the coalescing decides whether to keep
		// the window, so it can outlive one, and a stale instant would be
		// quoted as the loss of whatever round came next.
		d.recoveryMissingSince, d.recoveryMissingWhy = time.Time{}, ""
		return time.Time{}, "", time.Time{}, false
	}
	from, missing, missingWhy := d.recoveryFrom, d.recoveryMissingSince, d.recoveryMissingWhy
	// The measurement's own reason where there is one, because that is the
	// pair the trim warning quotes. The window's reason stands in only when
	// nothing measured anything, where the two are the same request.
	why := d.recoveryWhy
	if missingWhy != "" {
		why = missingWhy
	}
	d.recoveryFrom, d.recoveryWhy = time.Time{}, ""
	d.recoveryMissingSince, d.recoveryMissingWhy = time.Time{}, ""
	return from, why, missing, true
}

// requestDowntimeRecovery asks for a round covering a stretch nothing was
// watched for.
//
// It reads Since rather than Crashed, because a clean shutdown followed by
// a week with the machine off misses exactly as much as a power loss does.
// A first-ever start has no previous session and so no downtime, and the
// round a routine tick asks for is bounded by the library's first session,
// so neither path lets a fresh install fetch a channel's whole archive.
//
// Since is clamped before it is subtracted. A heartbeat far enough in the
// past, which a corrupt row or a zeroed clock leaves, saturates the
// duration, and negating a saturated one wraps it positive into a window
// that starts in the future.
func (d *Daemon) requestDowntimeRecovery(ctx context.Context, downtime Downtime, startedAt time.Time) {
	if downtime.Since < outageFloor {
		return
	}

	d.requestRecovery(startedAt.Add(-min(downtime.Since, recoveryHorizon)-recoveryMargin),
		fmt.Sprintf("nothing was watched for %s", roundDuration(downtime.Since)),
		startedAt.Add(-downtime.Since))
	// Said at startup as well as when the round runs, because the round
	// waits for the first answered probe and the wait is the part an
	// operator watching the log would otherwise read as nothing happening.
	d.logger.InfoContext(ctx, "queued a recovery round for the time nothing was watched",
		slog.Duration("down_for", roundDuration(downtime.Since)))
}
