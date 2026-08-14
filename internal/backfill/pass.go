package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Discovery lists a channel's past broadcasts and records what it finds.
// *Discoverer satisfies it.
type Discovery interface {
	Discover(ctx context.Context, channel Channel, now, since time.Time) (int, error)
}

// Fetching turns one candidate into a recording in the library.
// *Fetcher satisfies it.
type Fetching interface {
	Fetch(ctx context.Context, candidate Candidate, channel Channel, now time.Time) error
}

// Patching fills the holes inside a broadcast the recorder did capture.
// *Patcher satisfies it.
type Patching interface {
	Patch(ctx context.Context, broadcast store.Broadcast, channel Channel, now time.Time) (int, error)
}

// Outcome is one thing a pass did that is worth telling someone about.
//
// It is this package's own shape rather than the daemon's event, so the
// recorder can decide when a round runs without depending on the engine
// that runs one. Whoever wires the two together translates.
type Outcome struct {
	// Kind is one of the Outcome constants.
	Kind string
	// Channel is the channel involved.
	Channel string
	// Title is the broadcast's title, when it has one.
	Title string
	// Detail explains what happened.
	Detail string
}

// Result is what one pass achieved.
//
// Failed is the field a caller deciding whether to ask again reads. A pass
// reports every other failure per channel and carries on, so without it a
// round where every listing timed out and every fetch failed is
// indistinguishable from one that found the library already complete.
type Result struct {
	// Recovered counts the broadcasts fetched whole.
	Recovered int
	// GapsFilled counts the holes patched inside captured broadcasts.
	GapsFilled int
	// Failed counts the things that went wrong and might not go wrong next
	// time. A broadcast the pass gave up on is not one of them: that
	// decision is final and asking again would not change it.
	Failed int
	// Deferred counts the broadcasts something else was holding, so this
	// round could not try them. It is not a failure and nothing is wrong,
	// but the work is still outstanding: a caller that read only Failed
	// would call a round that fetched nothing at all a clean one and drop
	// the window it was covering.
	Deferred int
}

// tally accumulates a Result across the goroutines a pass fetches on.
type tally struct {
	mu     sync.Mutex
	counts Result
}

// PassDeps supplies what one round needs.
type PassDeps struct {
	// Coverage answers which days a channel is missing.
	Coverage Coverage
	// Discover lists past broadcasts. A nil discoverer plans from what the
	// recorder already saw.
	Discover Discovery
	// Fetch downloads a candidate.
	Fetch Fetching
	// Patch fills the holes inside broadcasts that were captured. A nil
	// patcher leaves them, because a hole can only be filled from an
	// archive copy.
	Patch Patching
	// Window bounds how far back a pass looks and how long it leaves a
	// broadcast alone after it ends.
	Window Window
	// MaxConcurrent bounds simultaneous fetches. Anything below one is
	// read as one.
	MaxConcurrent int
	// Logger records what a pass did.
	Logger *slog.Logger
	// Report carries an outcome to whatever tells the operator. Nil sends
	// nowhere, which leaves the log as the only account, and the log is
	// always written.
	//
	// A pass calls it once per candidate and never twice at the same time,
	// so an implementation may count what it is handed without a lock of
	// its own.
	Report func(Outcome)
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Outcome kinds, as reported.
const (
	// OutcomeRecovered names a broadcast fetched whole.
	OutcomeRecovered = "recovered"
	// OutcomeGapFilled names a hole patched inside a captured broadcast.
	OutcomeGapFilled = "gap_filled"
	// OutcomeGaveUp names a broadcast that will not be tried again.
	OutcomeGaveUp = "fetch_gave_up"
)

// ///////////////////////////////////////////////
// Pass
// ///////////////////////////////////////////////

// Pass runs one recovery round over every channel it is given.
//
// One channel's failure never stops the others. A listing is a request
// against somebody else's service, and a platform that is down, rate
// limiting, or has changed its markup takes every channel on it with it. A
// pass that stopped at the first would leave every later channel unfetched
// for as long as that lasted, which is the state backfill exists to end.
//
// The error is only the cancellation. Everything else is reported per
// channel and carried in the log, because a pass runs unattended on a timer
// and there is nobody to hand a failure to. The Result is what a caller
// reads instead, and its Failed count is how a recorder deciding whether to
// ask for this window again tells a bad round from a quiet one.
func Pass(ctx context.Context, deps PassDeps, channels []Channel, now time.Time) (Result, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	deps.Report = serialized(deps.Report)
	counts := &tally{}

	for _, channel := range channels {
		if ctx.Err() != nil {
			return counts.total(), ctx.Err()
		}
		passChannel(ctx, deps, logger, counts, channel, now)
	}
	return counts.total(), ctx.Err()
}

// recovered records one broadcast fetched whole.
func (t *tally) recovered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts.Recovered++
}

// filled records holes patched inside one broadcast.
func (t *tally) filled(holes int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts.GapsFilled += holes
}

// failed records something that might work on a later round.
func (t *tally) failed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts.Failed++
}

// deferred records a broadcast something else was holding.
func (t *tally) deferred() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts.Deferred++
}

// total returns what the pass has done so far.
func (t *tally) total() Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts
}

// passChannel discovers and fetches for one channel.
func passChannel(ctx context.Context, deps PassDeps, logger *slog.Logger,
	counts *tally, channel Channel, now time.Time,
) {
	if deps.Discover != nil {
		// A listing that failed is not a reason to skip the fetch. The
		// broadcasts the recorder already saw are in the database, and the
		// gaps they leave are fetchable without learning about new ones.
		// The lookback is the horizon listing walks back to. Without it the
		// listing is a count of the newest archives, which reaches roughly a
		// week on a channel that streams daily and never walks backward
		// however many passes run.
		written, err := deps.Discover.Discover(ctx, channel, now,
			now.Add(-deps.Window.withDefaults().Lookback))
		switch {
		case err != nil:
			counts.failed()
			logger.WarnContext(ctx, "could not list past broadcasts",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("error", escape.Field(err.Error())))
		case written > 0:
			logger.InfoContext(ctx, "discovered past broadcasts",
				slog.String("channel", escape.Field(channel.Name)),
				slog.Int("written", written))
		}
	}

	// Patching runs whatever the fetch plan says. A broadcast with a hole in
	// it is not a fetch candidate, precisely because something was captured.
	patchChannel(ctx, deps, logger, counts, channel, now)

	candidates, err := Candidates(deps.Coverage, channel.ID, now, deps.Window)
	if err != nil {
		counts.failed()
		logger.WarnContext(ctx, "could not plan a recovery",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("error", escape.Field(err.Error())))
		return
	}
	if len(candidates) == 0 {
		return
	}

	logger.InfoContext(ctx, "fetching what the recorder missed",
		slog.String("channel", escape.Field(channel.Name)),
		slog.Int("candidates", len(candidates)))

	fetchAll(ctx, deps, logger, counts, channel, candidates, now)
}

// patchChannel fills the holes inside broadcasts the recorder did capture.
//
// It runs over the broadcasts in the window rather than over the fetch
// candidates, because the two sets are disjoint. A candidate is a broadcast
// with nothing usable on disk. A patchable one has a capture with a hole in
// it, which is exactly what stops it being a candidate.
func patchChannel(ctx context.Context, deps PassDeps, logger *slog.Logger,
	counts *tally, channel Channel, now time.Time,
) {
	if deps.Patch == nil {
		return
	}

	window := deps.Window.withDefaults()
	broadcasts, err := deps.Coverage.BroadcastsBetween(channel.ID, now.Add(-window.Lookback), now)
	if err != nil {
		counts.failed()
		logger.WarnContext(ctx, "could not read broadcasts to patch",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("error", escape.Field(err.Error())))
		return
	}

	patched := 0
	for _, broadcast := range broadcasts {
		if ctx.Err() != nil {
			// Broken rather than returned, so the holes already filled are
			// still counted and still reported.
			break
		}
		// A broadcast that has not settled is one an archive copy may not
		// carry yet, and a patch of a range the platform has not published
		// downloads nothing.
		if !settled(broadcast, now, window.Settle) {
			continue
		}
		filled, err := deps.Patch.Patch(ctx, broadcast, channel, now)
		switch {
		case errors.Is(err, ErrNoAddress), errors.Is(err, ErrNoAnchor):
			// Every broadcast the recorder watched live sits here until
			// discovery reports where its stored copy lives and where that
			// copy's timeline begins, which is the ordinary state rather than
			// a fault.
			logger.DebugContext(ctx, "left a broadcast's holes alone",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("reason", escape.Field(err.Error())))
			continue
		case err != nil:
			counts.failed()
			logger.WarnContext(ctx, "could not patch a broadcast",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("error", escape.Field(err.Error())))
			continue
		}
		patched += filled
	}
	counts.filled(patched)

	if patched > 0 {
		logger.InfoContext(ctx, "filled holes inside captured broadcasts",
			slog.String("channel", escape.Field(channel.Name)),
			slog.Int("filled", patched))
		report(deps, Outcome{
			Kind:    OutcomeGapFilled,
			Channel: channel.Name,
			Detail:  fmt.Sprintf("patched %d %s", patched, plural(patched, "gap", "gaps")),
		})
	}
}

// plural picks the word for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// fetchAll fetches every candidate, no more than MaxConcurrent at once.
func fetchAll(ctx context.Context, deps PassDeps, logger *slog.Logger,
	counts *tally, channel Channel, candidates []Candidate, now time.Time,
) {
	if deps.Fetch == nil {
		return
	}

	slots := make(chan struct{}, max(deps.MaxConcurrent, 1))
	var wg sync.WaitGroup

	for _, candidate := range candidates {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			fetchOne(ctx, deps, logger, counts, channel, candidate, now)
		})
	}
	wg.Wait()
}

// report hands an outcome to whoever is listening, if anyone is.
func report(deps PassDeps, outcome Outcome) {
	if deps.Report == nil {
		return
	}
	deps.Report(outcome)
}

// serialized wraps a callback so one pass never runs two of them at once.
//
// A pass fetches up to MaxConcurrent candidates in their own goroutines and
// each reports its own outcome, so the callback is re-entered from as many
// goroutines as there are candidates. Serializing here means an
// implementation can count what it is handed without knowing that, which is
// what a caller writing `fetched++` reasonably assumes.
func serialized(report func(Outcome)) func(Outcome) {
	if report == nil {
		return nil
	}

	var mu sync.Mutex
	return func(outcome Outcome) {
		mu.Lock()
		defer mu.Unlock()
		report(outcome)
	}
}

// fetchOne fetches a single candidate and reports how it went.
func fetchOne(ctx context.Context, deps PassDeps, logger *slog.Logger,
	counts *tally, channel Channel, candidate Candidate, now time.Time,
) {
	err := deps.Fetch.Fetch(ctx, candidate, channel, now)

	switch {
	case err == nil:
		counts.recovered()
		logger.InfoContext(ctx, "recovered a broadcast",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("title", escape.Field(candidate.Broadcast.Title)))
		report(deps, Outcome{
			Kind:    OutcomeRecovered,
			Channel: channel.Name,
			Title:   candidate.Broadcast.Title,
			Detail:  "fetched a broadcast the recorder missed",
		})
	case errors.Is(err, ErrGaveUp):
		// The one outcome that does not resolve on its own, so it is the
		// one an operator is told about rather than left to find in a log.
		logger.WarnContext(ctx, "gave up on a broadcast",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("title", escape.Field(candidate.Broadcast.Title)))
		report(deps, Outcome{
			Kind:    OutcomeGaveUp,
			Channel: channel.Name,
			Title:   candidate.Broadcast.Title,
			Detail:  escape.Field(err.Error()),
		})
	case errors.Is(err, ErrNotClaimed):
		// Somebody else holds it, or its own backoff has not elapsed. The
		// broadcast is still outstanding, so a round that met only these
		// has not finished the work it was asked to do.
		counts.deferred()
		logger.DebugContext(ctx, "left a broadcast alone",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("reason", escape.Field(err.Error())))
	case errors.Is(err, ErrAlreadyCaptured), errors.Is(err, ErrNoAddress), errors.Is(err, ErrNoRoom):
		// All three are ordinary outcomes of a pass rather than faults: a
		// capture landed while this pass was planning, the platform has not yet
		// published the stored copy this broadcast would be fetched from, or
		// the library is full. The last one is the operator's to resolve and
		// resolves nothing sooner for being retried, so counting it as a
		// failure would have the recorder repeat the whole round every time
		// its pacing floor elapsed, for as long as the library stayed full.
		logger.DebugContext(ctx, "left a broadcast alone",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("reason", escape.Field(err.Error())))
	case ctx.Err() != nil:
		// A cancelled fetch downloaded nothing, and the fetcher has already
		// handed the broadcast back untouched, so the next round finds it
		// exactly as this one did.
	default:
		counts.failed()
		logger.WarnContext(ctx, "could not recover a broadcast",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("title", escape.Field(candidate.Broadcast.Title)),
			slog.String("error", escape.Field(err.Error())))
	}
}
