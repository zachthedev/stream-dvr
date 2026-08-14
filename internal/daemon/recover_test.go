package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// recoveryStub stands in for the round the command wires up.
//
// Rounds are collected rather than counted, because what a test checks is
// the window and the scope the daemon asked for, not that it asked at all.
type recoveryStub struct {
	mu     sync.Mutex
	rounds []Round
	result RoundResult
	err    error
	// announce is reported through the round's own callback, so a test can
	// check an outcome reaches the operator the way the recorder's other
	// events do.
	announce []Event
	// block holds a round open until it is closed, so a test can observe a
	// daemon that is mid-round.
	block chan struct{}
	// ran carries one send per round, so a test blocks on a round rather
	// than polling for one.
	ran chan Round
}

// messageLog keeps what a run logged, for a test whose subject is whether
// something was said and what figure it quoted.
type messageLog struct {
	mu       sync.Mutex
	messages []string
	attrs    []map[string]slog.Value
}

// roundTimeout is a backstop against a round that never runs, not a
// measurement of how long one takes. Every round here is a channel send.
const roundTimeout = 30 * time.Second

// run satisfies the daemon's recovery hook.
func (s *recoveryStub) run(ctx context.Context, round Round) (RoundResult, error) {
	s.mu.Lock()
	s.rounds = append(s.rounds, round)
	block, result, err := s.block, s.result, s.err
	announce := slices.Clone(s.announce)
	s.mu.Unlock()

	for _, event := range announce {
		round.Report(event)
	}

	select {
	case s.ran <- round:
	default:
	}

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return RoundResult{}, ctx.Err()
		}
	}
	return result, err
}

// count reports how many rounds have run.
func (s *recoveryStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rounds)
}

// last returns the most recent round, failing the test when none has run.
func (s *recoveryStub) last(t *testing.T) Round {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rounds) == 0 {
		t.Fatal("no recovery round ran, want one")
	}
	return s.rounds[len(s.rounds)-1]
}

// awaitRound blocks until a round runs.
func (s *recoveryStub) awaitRound(t *testing.T) Round {
	t.Helper()

	select {
	case round := <-s.ran:
		return round
	case <-time.After(roundTimeout):
		t.Fatal("no recovery round ran, want one")
		return Round{}
	}
}

// Enabled implements slog.Handler.
func (m *messageLog) Enabled(context.Context, slog.Level) bool { return true }

// WithAttrs implements slog.Handler.
func (m *messageLog) WithAttrs([]slog.Attr) slog.Handler { return m }

// WithGroup implements slog.Handler.
func (m *messageLog) WithGroup(string) slog.Handler { return m }

// Handle implements slog.Handler.
func (m *messageLog) Handle(_ context.Context, record slog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fields := map[string]slog.Value{}
	record.Attrs(func(attr slog.Attr) bool {
		fields[attr.Key] = attr.Value
		return true
	})
	m.messages = append(m.messages, record.Message)
	m.attrs = append(m.attrs, fields)
	return nil
}

// duration returns a duration attribute off the first message carrying
// text, failing the test when nothing logged one.
func (m *messageLog) duration(t *testing.T, text, key string) time.Duration {
	t.Helper()
	return m.attr(t, text, key).Duration()
}

// attr returns one attribute off the first message carrying text, failing
// the test when nothing logged one.
func (m *messageLog) attr(t *testing.T, text, key string) slog.Value {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()
	for at, message := range m.messages {
		if !strings.Contains(message, text) {
			continue
		}
		value, ok := m.attrs[at][key]
		if !ok {
			t.Fatalf("message %q carried no %q, want it reported", message, key)
		}
		return value
	}
	t.Fatalf("nothing logged mentioned %q", text)
	return slog.Value{}
}

// text returns a string attribute off the first message carrying text,
// failing the test when nothing logged one.
func (m *messageLog) text(t *testing.T, message, key string) string {
	t.Helper()
	return m.attr(t, message, key).String()
}

// mentions reports whether anything logged carried the given text.
func (m *messageLog) mentions(text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, message := range m.messages {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

// recoverable arms the harness with a recovery hook and returns it.
//
// The field is set directly rather than through Options, because a test
// needs the daemon and the stub to refer to each other and New takes the
// hook before either exists.
func (h *harness) recoverable() *recoveryStub {
	h.t.Helper()

	stub := &recoveryStub{ran: make(chan Round, 8)}
	h.daemon.recovery = stub.run
	return stub
}

// recordedSince makes the library look as though a recorder first claimed
// it at the given instant.
//
// Automatic recovery reaches no further back than that, so a test wanting a
// round to cover anything has to say when the library started existing. The
// session is closed again, so it leaves a first start on record and no
// claim behind it.
func (h *harness) recordedSince(at time.Time) {
	h.t.Helper()

	h.clockMu.Lock()
	resume := h.clock
	h.clockMu.Unlock()

	h.at(at)
	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		h.t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if err := h.daemon.StopSession(session.ID); err != nil {
		h.t.Fatalf("StopSession() err = %v, want nil", err)
	}
	h.at(resume)
}

// longRecorded puts the library's first session far enough back that the
// horizon, and not the library's own age, is what bounds a round.
func (h *harness) longRecorded() {
	h.t.Helper()
	h.recordedSince(now.Add(-recoveryHorizon - 24*time.Hour))
}

// reachable records that the harness channel's platform answered, which is
// what releases a round waiting on connectivity.
func (h *harness) reachable() {
	h.daemon.noteProbe(h.channel.ID, config.PlatformTwitch, true)
}

// unreachable records that the harness channel's platform did not answer.
func (h *harness) unreachable() {
	h.daemon.noteProbe(h.channel.ID, config.PlatformTwitch, false)
}

// outageOpen reports whether the daemon is holding an unfinished outage for
// the harness channel.
func (h *harness) outageOpen() bool {
	h.daemon.recoveryMu.Lock()
	defer h.daemon.recoveryMu.Unlock()
	_, open := h.daemon.outages[h.channel.ID]
	return open
}

// probeRecord reports what the daemon remembers of the harness channel's
// last probe, and whether it remembers one at all. The second answer is a
// different thing from the first: a channel nobody has probed is not a
// channel that failed.
func (h *harness) probeRecord() (probe, bool) {
	h.daemon.recoveryMu.Lock()
	defer h.daemon.recoveryMu.Unlock()
	last, seen := h.daemon.probed[h.channel.ID]
	return last, seen
}

// ///////////////////////////////////////////////
// What asks for a round
// ///////////////////////////////////////////////

func TestRequestDowntimeRecovery_ReachesBackPastEveryHourNothingWasWatched(t *testing.T) {
	// The operator's case: the machine lost power and came back days later.
	// A window starting when the recorder restarted recovers nothing, and
	// one starting when it died still misses the broadcast that was already
	// running when the power went.
	cases := []struct {
		name string
		down time.Duration
	}{
		{name: "a restart over lunch", down: 2 * time.Hour},
		{name: "overnight", down: 14 * time.Hour},
		{name: "a long weekend", down: 3 * 24 * time.Hour},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)

			h.daemon.requestDowntimeRecovery(context.Background(),
				Downtime{Since: testCase.down}, now)

			from, why, _, pending := h.daemon.takeRecovery()
			if !pending {
				t.Fatal("no round was queued, want one covering the downtime")
			}
			if want := now.Add(-testCase.down - recoveryMargin); !from.Equal(want) {
				t.Errorf("round covers from %v, want %v", from, want)
			}
			if why == "" {
				t.Error("the round carries no reason, so the log cannot say why it ran")
			}
		})
	}
}

func TestRequestDowntimeRecovery_TreatsACleanShutdownAsMissingJustAsMuch(t *testing.T) {
	// A recorder stopped deliberately and started a week later missed every
	// broadcast in between, exactly as a crashed one did. Reading Crashed
	// here would leave the orderly case silently uncovered.
	h := newHarness(t, nil)

	h.daemon.requestDowntimeRecovery(context.Background(),
		Downtime{Since: 7 * 24 * time.Hour, Crashed: false}, now)

	if _, _, _, pending := h.daemon.takeRecovery(); !pending {
		t.Error("no round was queued after a clean shutdown, want one")
	}
}

func TestRequestDowntimeRecovery_AsksForNothingAfterARestartTooShortToMiss(t *testing.T) {
	// Restarting the service, or a laptop lid, must not queue a round that
	// lists every channel to find exactly nothing.
	h := newHarness(t, nil)

	h.daemon.requestDowntimeRecovery(context.Background(),
		Downtime{Since: outageFloor - time.Second, Crashed: true}, now)

	if _, _, _, pending := h.daemon.takeRecovery(); pending {
		t.Error("a round was queued for a restart shorter than the floor, want none")
	}
}

func TestRequestDowntimeRecovery_SurvivesADowntimeThatSaturatedTheClock(t *testing.T) {
	// A heartbeat far enough in the past saturates the subtraction, which a
	// corrupt row or a zeroed clock leaves behind. Negating a saturated
	// duration wraps it positive, and the window would start in the future.
	h := newHarness(t, nil)

	h.daemon.requestDowntimeRecovery(context.Background(),
		Downtime{Since: time.Duration(1<<63 - 1)}, now)

	from, _, _, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("no round was queued for a saturated downtime, want one")
	}
	if !from.Before(now) {
		t.Errorf("round covers from %v, which is not before %v", from, now)
	}
}

func TestRequestRecovery_KeepsWhicheverWindowReachesFurthestBack(t *testing.T) {
	// Several channels coming back from one outage, or an outage ending
	// while a round is already queued, must produce one round rather than a
	// queue of them, and it must be the one that covers all of it.
	h := newHarness(t, nil)

	// A distinct measured start per request. Coverage stopping earliest is
	// what sizes the loss the operator is warned about, and it is kept
	// whichever window wins: dropping it along with a losing request is a
	// round that trims real coverage and never says so.
	h.daemon.requestRecovery(now.Add(-2*time.Hour), "the nearer one", now.Add(-time.Hour))
	h.daemon.requestRecovery(now.Add(-9*time.Hour), "the further one", now.Add(-8*time.Hour))
	h.daemon.requestRecovery(now.Add(-time.Hour), "nearer again", now.Add(-30*time.Minute))

	from, why, missing, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("no round was queued, want one")
	}
	if want := now.Add(-9 * time.Hour); !from.Equal(want) {
		t.Errorf("round covers from %v, want %v", from, want)
	}
	if why != "the further one" {
		t.Errorf("reason = %q, want the reason belonging to the widest window", why)
	}
	if want := now.Add(-8 * time.Hour); !missing.Equal(want) {
		t.Errorf("missing since %v, want the earliest measured start %v", missing, want)
	}
	if _, _, _, again := h.daemon.takeRecovery(); again {
		t.Error("a second round was queued, want the request taken exactly once")
	}
}

func TestReopenSession_QueuesARoundForTheStretchAFreezeHid(t *testing.T) {
	// A sleeping laptop or a suspended VM leaves the process alive, so the
	// next start measures its downtime from a heartbeat written seconds ago
	// and asks for nothing. The freeze is the one gap a restart cannot
	// report, and this is where its length is known.
	h := newHarness(t, nil)
	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	h.daemon.setSession(session.ID)

	frozenFor := 3 * 24 * time.Hour
	h.at(now.Add(frozenFor))
	h.daemon.reopenSession(context.Background(), now, now.Add(frozenFor))

	from, _, _, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("no round was queued for the frozen stretch, want one")
	}
	if want := now.Add(-recoveryMargin); !from.Equal(want) {
		t.Errorf("round covers from %v, want %v", from, want)
	}
}

// ///////////////////////////////////////////////
// Outages
// ///////////////////////////////////////////////

func TestNoteProbe_AsksForARoundCoveringAnOutageThatEnded(t *testing.T) {
	h := newHarness(t, nil)

	h.unreachable()
	h.at(now.Add(40 * time.Minute))
	h.reachable()

	from, _, _, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("no round was queued when the platform came back, want one")
	}
	// Measured from when the outage began, not from when it ended, and
	// extended by the margin so a broadcast already running when the network
	// dropped is still in range.
	if want := now.Add(-recoveryMargin); !from.Equal(want) {
		t.Errorf("round covers from %v, want %v", from, want)
	}
}

func TestNoteProbe_DatesAnOutageFromItsFirstFailureAndNotItsLast(t *testing.T) {
	// An outage re-dated on every failed poll is always measured as zero,
	// which reads as no outage at all.
	h := newHarness(t, nil)

	h.unreachable()
	for minute := range 30 {
		h.at(now.Add(time.Duration(minute+1) * time.Minute))
		h.unreachable()
	}
	h.at(now.Add(31 * time.Minute))
	h.reachable()

	from, _, _, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("no round was queued after a run of failures, want one")
	}
	if want := now.Add(-recoveryMargin); !from.Equal(want) {
		t.Errorf("round covers from %v, want %v", from, want)
	}
}

func TestNoteProbe_IgnoresAFailureTooBriefToHaveHiddenABroadcast(t *testing.T) {
	h := newHarness(t, nil)

	h.unreachable()
	h.at(now.Add(outageFloor - time.Second))
	h.reachable()

	if _, _, _, pending := h.daemon.takeRecovery(); pending {
		t.Error("a round was queued for one refused probe, want none")
	}
}

func TestNoteProbe_CoalescesEveryChannelComingBackIntoOneRound(t *testing.T) {
	// A network outage fails every channel at once. Each one recovering
	// must not queue a round of its own: the fetches would race and the
	// listings would be requested several times over.
	h := newHarness(t, nil)
	const channels = 4

	for id := range int64(channels) {
		h.daemon.noteProbe(id+1, config.PlatformTwitch, false)
	}
	h.at(now.Add(time.Hour))
	for id := range int64(channels) {
		h.daemon.noteProbe(id+1, config.PlatformTwitch, true)
	}

	from, _, _, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("no round was queued, want one")
	}
	if want := now.Add(-recoveryMargin); !from.Equal(want) {
		t.Errorf("round covers from %v, want %v", from, want)
	}
	if _, _, _, again := h.daemon.takeRecovery(); again {
		t.Error("a second round was queued, want every channel to share one")
	}
}

// ///////////////////////////////////////////////
// Reachability is per platform
// ///////////////////////////////////////////////

func TestReachablePlatforms_NamesOnlyTheServicesThatAnswered(t *testing.T) {
	// One service being up says nothing about another. A round that fetched
	// against the one that is down would charge an attempt per broadcast
	// there, and a broadcast that spends its last one is retired for good.
	h := newHarness(t, nil)

	h.daemon.noteProbe(1, config.PlatformTwitch, true)
	h.daemon.noteProbe(2, config.PlatformTwitch, true)
	h.daemon.noteProbe(3, config.PlatformYouTube, false)

	got := h.daemon.reachablePlatforms()

	if want := []string{config.PlatformTwitch}; !slices.Equal(got, want) {
		t.Errorf("reachablePlatforms() = %v, want %v", got, want)
	}
}

func TestReachablePlatforms_CountsAServiceUpWhenAnyOfItsChannelsAnswers(t *testing.T) {
	// A channel that was deleted or banned fails forever while the service
	// itself is fine. Judging the platform by its worst channel would hold
	// recovery for every other channel on it.
	h := newHarness(t, nil)

	h.daemon.noteProbe(1, config.PlatformTwitch, false)
	h.daemon.noteProbe(2, config.PlatformTwitch, true)

	if got := h.daemon.reachablePlatforms(); !slices.Contains(got, config.PlatformTwitch) {
		t.Errorf("reachablePlatforms() = %v, want twitch counted as reachable", got)
	}
}

func TestRunRecovery_ScopesTheRoundToThePlatformsThatAnswered(t *testing.T) {
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()

	h.daemon.noteProbe(1, config.PlatformTwitch, true)
	h.daemon.noteProbe(2, config.PlatformYouTube, false)
	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	round := stub.last(t)
	if want := []string{config.PlatformTwitch}; !slices.Equal(round.Platforms, want) {
		t.Errorf("round covers %v, want %v", round.Platforms, want)
	}
}

// ///////////////////////////////////////////////
// What holds a round
// ///////////////////////////////////////////////

func TestRunRecovery_SpendsNoFetchAttemptsWhileTheresNothingToReach(t *testing.T) {
	// The dangerous case. A fetch that fails charges an attempt against the
	// broadcast, and at the configured cap the store is told never to offer
	// it again. A round run while the platform is unreachable therefore
	// does not merely fail: it spends the only chance those broadcasts had.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()

	h.unreachable()
	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if stub.count() != 0 {
		t.Errorf("rounds = %d, want none while no channel can be reached", stub.count())
	}
	if _, _, _, pending := h.daemon.takeRecovery(); !pending {
		t.Error("the held round was discarded, want it kept for when the platform answers")
	}
}

func TestRunRecovery_HoldsUntilAChannelHasActuallyBeenProbed(t *testing.T) {
	// A restart queues its round before any watcher has run. A channel
	// nobody has probed is not a channel that answered, and treating the
	// two alike would fetch across a machine whose network is not up yet.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()

	h.daemon.requestRecovery(now.Add(-time.Hour), "a restart", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())
	if stub.count() != 0 {
		t.Fatalf("rounds = %d, want none before any probe has been answered", stub.count())
	}

	h.reachable()
	h.daemon.runRecovery(context.Background())
	if stub.count() != 1 {
		t.Errorf("rounds = %d, want one once a probe was answered", stub.count())
	}
}

func TestRunRecovery_HoldsWhileACaptureIsRunning(t *testing.T) {
	// A download and a capture write to one volume and pull one link, and
	// only the capture is irreplaceable. The watermark halts the capture
	// and nothing halts an admitted download, so a round running alongside
	// one can end the recording it was meant to complement.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()
	h.daemon.noteCapturing(7)

	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())
	if stub.count() != 0 {
		t.Fatalf("rounds = %d, want none while a capture is writing", stub.count())
	}

	h.daemon.forgetCapturing(7)
	h.daemon.runRecovery(context.Background())
	if stub.count() != 1 {
		t.Errorf("rounds = %d, want one once the capture ended", stub.count())
	}
}

func TestNoteCapturing_StopsARoundAlreadyInFlight(t *testing.T) {
	// The hold is checked once, before the round starts, and a round runs
	// for hours. A broadcast beginning an hour in would otherwise leave a
	// download competing with the recording for the disk and the link, and
	// the watermark ends the capture while nothing ends the download.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.block = make(chan struct{})
	h.longRecorded()
	h.reachable()

	from := now.Add(-time.Hour)
	h.daemon.requestRecovery(from, "an outage", now.Add(-time.Hour))

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.runRecovery(context.Background())
	}()

	stub.awaitRound(t)
	h.daemon.noteCapturing(11)

	select {
	case <-done:
	case <-time.After(roundTimeout):
		t.Fatal("the round is still running, want the capture to have stopped it")
	}

	// The window it was covering has to come back, or a capture starting
	// mid round would cost the recovery outright.
	if queued, _, _, pending := h.daemon.takeRecovery(); !pending || !queued.Equal(from) {
		t.Errorf("requeued from %v pending=%v, want the window kept at %v", queued, pending, from)
	}
}

func TestRunRecovery_PacesRoundsApartSoAFlappingLinkCannotDriveThem(t *testing.T) {
	// A link that drops and returns every few minutes queues a round each
	// time it comes back, and each round is a listing request per channel.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()

	h.daemon.requestRecovery(now.Add(-time.Hour), "the first outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	h.at(now.Add(recoveryFloor - time.Minute))
	h.daemon.requestRecovery(now.Add(-time.Hour), "the link dropped again", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())
	if stub.count() != 1 {
		t.Fatalf("rounds = %d, want the second one paced out", stub.count())
	}

	h.at(now.Add(recoveryFloor + time.Minute))
	h.daemon.runRecovery(context.Background())
	if stub.count() != 2 {
		t.Errorf("rounds = %d, want the held round to run once the floor passed", stub.count())
	}
}

func TestRecoveryHeldBy_SaysWhichConditionIsHoldingTheRound(t *testing.T) {
	// A round that is held says so in the log, and a message naming no
	// cause leaves an operator watching a recorder that fetches nothing
	// with nothing to read.
	h := newHarness(t, nil)

	if held := h.daemon.recoveryHeldBy(h.daemon.reachablePlatforms()); held == "" {
		t.Error("nothing is reported as holding the round, want the unreachable platform named")
	}

	h.reachable()
	if held := h.daemon.recoveryHeldBy(h.daemon.reachablePlatforms()); held != "" {
		t.Errorf("held by %q, want nothing once a probe was answered", held)
	}

	h.daemon.noteCapturing(3)
	if held := h.daemon.recoveryHeldBy(h.daemon.reachablePlatforms()); held == "" {
		t.Error("nothing is reported as holding the round, want the running capture named")
	}
}

func TestRecoveryHeldBy_DoesNotHoldForACredentialTheFetchPathNeverUses(t *testing.T) {
	// The recording credential is streamlink's. A recovery fetch runs
	// yt-dlp against a public archive and carries no credential at all, so
	// holding on that condition would stop recovery over something it does
	// not need. The latch is one-way besides: the refusal deletes the file,
	// every later check answers absent, and only a credential that
	// validates clears it. A recorder capturing at public quality for a
	// week would then recover nothing for that week either.
	h := newHarness(t, nil)
	h.reachable()
	h.daemon.latchCredential(now, true)

	if held := h.daemon.recoveryHeldBy(h.daemon.reachablePlatforms()); held != "" {
		t.Errorf("held by %q, want a refused recording credential not to hold recovery", held)
	}
}

func TestRunRecovery_ReportsAHoldThatNeverClears(t *testing.T) {
	// Every hold is meant to clear on its own. One that does not, which a
	// channel misspelled in the config causes permanently, leaves a
	// recorder recovering nothing and looking exactly like one with nothing
	// to recover.
	h := newHarness(t, func(cfg *config.Config) { cfg.Notify.OnFailure = true })
	h.recoverable()
	h.longRecorded()

	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())
	if h.notifier.has(EventFailure) {
		t.Fatalf("events = %v, want a hold reported only once it persists", h.notifier.kinds())
	}

	h.at(now.Add(holdReportAfter + time.Minute))
	h.daemon.runRecovery(context.Background())
	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want the persistent hold reported", h.notifier.kinds())
	}

	// Not on every poll. A recorder that cannot reach anything for a week
	// must not raise a notification per channel per poll for that week.
	h.at(now.Add(holdReportAfter + 2*time.Minute))
	h.daemon.runRecovery(context.Background())
	if got := h.notifier.count(EventFailure); got != 1 {
		t.Errorf("reported the hold %d times inside one interval, want once", got)
	}

	// Said again once the interval passes. A hold that never clears is the
	// one an operator most needs reminding of, and a single notification
	// they happened to miss would be the only signal there ever was.
	h.at(now.Add(2*holdReportAfter + 2*time.Minute))
	h.daemon.runRecovery(context.Background())
	if got := h.notifier.count(EventFailure); got != 2 {
		t.Errorf("reported the hold %d times, want it repeated once the interval passed", got)
	}
}

// ///////////////////////////////////////////////
// Rounds
// ///////////////////////////////////////////////

func TestRunRecovery_QueuesTheWindowAgainWhenARoundFails(t *testing.T) {
	// The request is taken before the work runs, so a round that fails
	// consumes the window it was covering.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.err = errors.New("the archive could not be listed")
	h.longRecorded()
	h.reachable()

	from := now.Add(-9 * 24 * time.Hour)
	h.daemon.requestRecovery(from, "a long outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if stub.count() != 1 {
		t.Fatalf("rounds = %d, want the failing round to have run", stub.count())
	}
	queued, _, _, pending := h.daemon.takeRecovery()
	if !pending {
		t.Fatal("the failed round consumed its window, want it queued again")
	}
	if !queued.Equal(from) {
		t.Errorf("requeued from %v, want the window the round was covering, %v", queued, from)
	}
}

func TestRunRecovery_QueuesTheWindowAgainWhenPartOfARoundFailed(t *testing.T) {
	// A pass reports its failures per channel and returns only the
	// cancellation, so a round where every listing timed out and every
	// fetch failed comes back with a nil error. Read on the error alone it
	// looks like a library already complete, and the window is dropped.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.result = RoundResult{Recovered: 1, Failed: 3}
	h.longRecorded()
	h.reachable()

	from := now.Add(-9 * 24 * time.Hour)
	h.daemon.requestRecovery(from, "a long outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if queued, _, _, pending := h.daemon.takeRecovery(); !pending || !queued.Equal(from) {
		t.Errorf("requeued from %v pending=%v, want the window kept at %v", queued, pending, from)
	}
}

func TestRunRecovery_KeepsNoWindowWhenEverythingWorked(t *testing.T) {
	// The other half. A round that did its work must not queue itself
	// again, or the recorder fetches the same window forever.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.result = RoundResult{Recovered: 2}
	h.longRecorded()
	h.reachable()

	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if _, _, _, pending := h.daemon.takeRecovery(); pending {
		t.Error("a window was queued again after a round that worked, want none")
	}
}

func TestRunRecovery_QueuesTheWindowAgainWhenTheRecorderShutsDown(t *testing.T) {
	// A reboot part way through the round that follows a long outage would
	// otherwise lose that window: the restart measures downtime from a
	// heartbeat written moments ago and reports seconds.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.err = context.Canceled
	h.longRecorded()
	h.reachable()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	from := now.Add(-9 * 24 * time.Hour)
	h.daemon.requestRecovery(from, "a long outage", now.Add(-time.Hour))
	h.daemon.runRecovery(ctx)

	if stub.count() != 1 {
		t.Fatalf("rounds = %d, want the cancelled round to have started", stub.count())
	}
	if queued, _, _, pending := h.daemon.takeRecovery(); !pending || !queued.Equal(from) {
		t.Errorf("requeued from %v pending=%v, want the window kept at %v", queued, pending, from)
	}
}

func TestRunRecovery_ClampsAGapLongerThanAutomaticRecoveryReachesBack(t *testing.T) {
	// A machine that lost power and came back a month later reports a month
	// of downtime. Taken literally that lists an entire archive and
	// downloads it unattended onto a volume sized for a fortnight.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()

	h.daemon.requestRecovery(now.Add(-90*24*time.Hour), "the machine was off for months",
		now.Add(-90*24*time.Hour))
	h.daemon.runRecovery(context.Background())

	round := stub.last(t)
	if want := now.Add(-recoveryHorizon); !round.Since.Equal(want) {
		t.Errorf("round covers from %v, want it clamped to %v", round.Since, want)
	}
}

func TestRunRecovery_ReachesNoFurtherBackThanTheLibraryItself(t *testing.T) {
	// A recorder cannot have missed a broadcast that aired before it
	// existed. The routine round asks for the whole horizon outright, so
	// without this a fresh install downloads a channel's entire archive
	// six hours after it was set up.
	h := newHarness(t, nil)
	stub := h.recoverable()
	firstRan := now.Add(-2 * 24 * time.Hour)
	h.recordedSince(firstRan)
	h.reachable()

	h.daemon.requestRecovery(now.Add(-recoveryHorizon), "the routine round", time.Time{})
	h.daemon.runRecovery(context.Background())

	if round := stub.last(t); !round.Since.Equal(firstRan) {
		t.Errorf("round covers from %v, want it bounded to %v", round.Since, firstRan)
	}
}

func TestRunRecovery_RunsNothingOnALibraryNoRecorderHasEverHeld(t *testing.T) {
	// Before the first start there is no history at all, so there is
	// nothing that could have been missed.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.reachable()

	h.daemon.requestRecovery(now.Add(-recoveryHorizon), "the routine round", time.Time{})
	h.daemon.runRecovery(context.Background())

	if stub.count() != 0 {
		t.Errorf("rounds = %d, want none on a library nothing has recorded to", stub.count())
	}
}

func TestRunRecovery_LeavesAGapInsideTheHorizonExactlyWhereItWasAskedFor(t *testing.T) {
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()

	from := now.Add(-recoveryHorizon + time.Hour)
	h.daemon.requestRecovery(from, "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if round := stub.last(t); !round.Since.Equal(from) {
		t.Errorf("round covers from %v, want %v", round.Since, from)
	}
}

func TestRunRecovery_RefusesAWindowThatEndsBeforeItStarts(t *testing.T) {
	// What a clock stepping backwards leaves behind. The planner reads an
	// empty range as unset and substitutes a month, so it must not be
	// handed one.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()

	h.daemon.requestRecovery(now.Add(2*time.Hour), "a clock that stepped back", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if stub.count() != 0 {
		t.Errorf("rounds = %d, want none for a window that ends before it starts", stub.count())
	}
}

func TestRunRecovery_HandsTheRoundTheSessionTheClaimIsHeldUnder(t *testing.T) {
	// A fetch records the session it ran under, so a crash mid-download
	// leaves a claim that expires with that session. A freeze replaces the
	// row mid-run, which is why the round is told at call time rather than
	// being built around whichever id the daemon started with.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()
	h.daemon.setSession(41)

	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())
	if got := stub.last(t).Session; got != 41 {
		t.Errorf("round ran under session %d, want 41", got)
	}

	h.at(now.Add(recoveryFloor + time.Minute))
	h.daemon.setSession(42)
	h.daemon.requestRecovery(now.Add(-time.Hour), "another outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())
	if got := stub.last(t).Session; got != 42 {
		t.Errorf("round ran under session %d, want the session in force now", got)
	}
}

func TestRunRecovery_RunsNothingWithNoRoundQueued(t *testing.T) {
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.reachable()

	h.daemon.runRecovery(context.Background())

	if stub.count() != 0 {
		t.Errorf("rounds = %d, want none with nothing queued", stub.count())
	}
}

// ///////////////////////////////////////////////
// What a round tells the operator
// ///////////////////////////////////////////////

// ///////////////////////////////////////////////
// The loop
// ///////////////////////////////////////////////

func TestRecoveryLoop_RunsARoundOnItsOwnCadenceWithNothingHavingGoneWrong(t *testing.T) {
	// A broadcast shorter than one poll, a capture that failed, and a hole
	// inside a capture are all missed with no outage anywhere. Nothing else
	// would ever ask for those. It reaches the whole horizon, because a
	// window an interrupted round was covering has nothing else to bring it
	// back once the process has gone.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.daemon.recoveryInterval = 5 * time.Millisecond
	h.reachable()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.recoveryLoop(ctx)
	}()

	round := stub.awaitRound(t)
	cancel()
	<-done

	if want := now.Add(-recoveryHorizon); !round.Since.Equal(want) {
		t.Errorf("routine round covers from %v, want %v", round.Since, want)
	}
}

func TestRecoveryLoop_StartsAHeldRoundWhenTheFirstProbeIsAnswered(t *testing.T) {
	// The restart path end to end. The round is queued before any watcher
	// runs and has nothing but a probe to release it, so without the poke a
	// reboot would wait out the routine cadence before recovering anything.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	h.daemon.recoveryInterval = time.Hour
	h.daemon.requestDowntimeRecovery(context.Background(), Downtime{Since: 6 * time.Hour}, now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.recoveryLoop(ctx)
	}()

	h.reachable()
	round := stub.awaitRound(t)
	cancel()
	<-done

	if want := now.Add(-6*time.Hour - recoveryMargin); !round.Since.Equal(want) {
		t.Errorf("round covers from %v, want %v", round.Since, want)
	}
}

func TestRecoveryLoop_StopsWhenTheRecorderIsShuttingDown(t *testing.T) {
	// A round can run for hours. Cancelling has to reach it, or every stop
	// waits out a download.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.block = make(chan struct{})
	h.longRecorded()
	h.daemon.recoveryInterval = 5 * time.Millisecond
	h.reachable()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.recoveryLoop(ctx)
	}()

	stub.awaitRound(t)
	cancel()

	select {
	case <-done:
	case <-time.After(roundTimeout):
		t.Fatal("the recovery loop is still running, want it to stop with the recorder")
	}
}

func TestRecoveryLoop_ReturnsAtOnceWithNoRecoveryWiredUp(t *testing.T) {
	// A daemon built without the hook must not sit on a ticker forever, and
	// Run waits for every loop it started.
	h := newHarness(t, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.recoveryLoop(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(roundTimeout):
		t.Fatal("the recovery loop is still running, want it to return with no hook to call")
	}
}

// ///////////////////////////////////////////////
// Wired into a run
// ///////////////////////////////////////////////

func TestRun_RecoversWhatWasMissedWhileTheMachineWasOff(t *testing.T) {
	// The whole path the operator asked about: the machine lost power, came
	// back days later, and the recorder fetches the broadcasts that aired
	// while it was gone without anybody typing a command.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()

	// A session that stopped beating four days ago, which is the only trace
	// a power loss leaves.
	crashed, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	h.at(now.Add(2 * time.Hour))
	if err := h.daemon.Heartbeat(crashed.ID); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}
	h.at(now.Add(96 * time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.daemon.Run(ctx) }()

	round := stub.awaitRound(t)
	cancel()
	if runErr := <-done; runErr != nil {
		t.Fatalf("Run() err = %v, want nil", runErr)
	}

	// Ninety-four hours between the last heartbeat and this start, reaching
	// back the margin further so a broadcast already running when the power
	// went is still in range.
	if want := now.Add(2*time.Hour - recoveryMargin); !round.Since.Equal(want) {
		t.Errorf("round covers from %v, want %v", round.Since, want)
	}
}

func TestRun_RecoversNothingOnAFirstEverStart(t *testing.T) {
	// A library nobody has recorded into has no previous session and so no
	// downtime to measure. Reading that as a gap would have every fresh
	// install open by downloading a channel's entire archive.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.live("a broadcast")
	h.captured(4*config.Megabyte.Bytes(), time.Hour)

	// The wait fails the test itself, naming which way it went wrong.
	h.notifier.waitFor(t, h.daemon, EventRecordingStarted)

	if stub.count() != 0 {
		t.Errorf("rounds = %d, want none on a first-ever start", stub.count())
	}
}

// ///////////////////////////////////////////////
// How a round paces the next one
// ///////////////////////////////////////////////

func TestNoteRoundRan_BacksOffOnFailureAndRecoversOnACleanRound(t *testing.T) {
	// A round that failed queues its window again, so without a ladder one
	// channel whose listing always fails turns the six-hour cadence into a
	// permanent thirty-minute one. Every round lists every channel, so that
	// is a standing twelvefold rise in requests against somebody else's
	// service, driven by one bad line of config.
	h := newHarness(t, nil)

	if got := h.daemon.roundPacing(); got != recoveryFloor {
		t.Errorf("pacing starts at %s, want %s", got, recoveryFloor)
	}

	// The first failure already costs something, or a round that failed
	// would be paced exactly like one that worked.
	want := []time.Duration{2 * recoveryFloor, 4 * recoveryFloor, 8 * recoveryFloor}
	for _, expected := range want {
		h.daemon.noteRoundRan(false)
		if got := h.daemon.roundPacing(); got != expected {
			t.Errorf("pacing after a failed round = %s, want %s", got, expected)
		}
	}

	// It never outgrows the routine cadence, or a failing library would be
	// left further apart than a healthy one is looked at.
	for range 20 {
		h.daemon.noteRoundRan(false)
	}
	if got := h.daemon.roundPacing(); got != recoveryInterval {
		t.Errorf("pacing = %s, want it capped at the routine interval %s", got, recoveryInterval)
	}

	h.daemon.noteRoundRan(true)
	if got := h.daemon.roundPacing(); got != recoveryFloor {
		t.Errorf("pacing after a clean round = %s, want it back to %s", got, recoveryFloor)
	}
}

func TestRunRecovery_DoesNotPaceACaptureYieldAsAFailure(t *testing.T) {
	// On a channel that streams most days every round ends this way. Charged
	// as failures they would push the cadence to its ceiling and leave it
	// there, so the recorder that records the most would recover the least.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.err = context.Canceled
	h.longRecorded()
	h.reachable()

	from := now.Add(-time.Hour)
	h.daemon.requestRecovery(from, "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if got := h.daemon.roundPacing(); got != recoveryFloor {
		t.Errorf("pacing after a capture yield = %s, want it left at %s", got, recoveryFloor)
	}
	if queued, _, _, pending := h.daemon.takeRecovery(); !pending || !queued.Equal(from) {
		t.Errorf("requeued from %v pending=%v, want the window kept at %v", queued, pending, from)
	}
}

func TestRunRecovery_KeepsTheWindowWhenEveryBroadcastWasHeldElsewhere(t *testing.T) {
	// Nothing went wrong and nothing got done. Read on the error alone that
	// is a clean round, and the window a restart measured is consumed
	// having fetched none of it, with the log reporting success.
	h := newHarness(t, nil)
	stub := h.recoverable()
	stub.result = RoundResult{Deferred: 3}
	h.longRecorded()
	h.reachable()

	from := now.Add(-9 * 24 * time.Hour)
	h.daemon.requestRecovery(from, "a long outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if queued, _, _, pending := h.daemon.takeRecovery(); !pending || !queued.Equal(from) {
		t.Errorf("requeued from %v pending=%v, want the window kept at %v", queued, pending, from)
	}
	// Asking again sooner would not change the answer, so the cadence backs
	// off rather than repeating a round that could take nothing.
	if got := h.daemon.roundPacing(); got == recoveryFloor {
		t.Error("pacing was left at the floor after a round that took nothing, want it backed off")
	}
}

func TestRunRecovery_ReportsARoundThatRanOutOfTime(t *testing.T) {
	// The loop is one goroutine on purpose, so a download that stalls
	// without ever failing stops recovery for good. The hold report covers
	// only a round that never started, so this is the one signal there is.
	h := newHarness(t, func(cfg *config.Config) { cfg.Notify.OnFailure = true })
	stub := h.recoverable()
	stub.err = context.DeadlineExceeded
	h.longRecorded()
	h.reachable()

	from := now.Add(-time.Hour)
	h.daemon.requestRecovery(from, "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want the stalled round reported", h.notifier.kinds())
	}
	if queued, _, _, pending := h.daemon.takeRecovery(); !pending || !queued.Equal(from) {
		t.Errorf("requeued from %v pending=%v, want the window kept at %v", queued, pending, from)
	}
	if got := h.daemon.roundPacing(); got == recoveryFloor {
		t.Error("pacing was left at the floor after a round ran out of time, want it backed off")
	}
}

// ///////////////////////////////////////////////
// What a round tells the operator, once
// ///////////////////////////////////////////////

func TestSummarizeRound_TellsTheOperatorOnceForTheWholeRound(t *testing.T) {
	// A round after a long outage recovers broadcasts by the dozen. One
	// notification each is a burst nobody reads, and on a desktop sink it
	// is a burst nobody can dismiss either.
	tests := []struct {
		name   string
		result RoundResult
		want   int
	}{
		{name: "nothing to say", result: RoundResult{}},
		{name: "one broadcast", result: RoundResult{Recovered: 1}, want: 1},
		{name: "a whole outage", result: RoundResult{Recovered: 12, GapsFilled: 3}, want: 1},
		{name: "gaps only", result: RoundResult{GapsFilled: 4}, want: 1},
		{name: "nothing fetched but work failed", result: RoundResult{Failed: 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)

			h.daemon.summarizeRound(context.Background(), tt.result)

			if got := h.notifier.count(EventRecovered); got != tt.want {
				t.Errorf("raised %d notifications, want %d", got, tt.want)
			}
		})
	}
}

func TestSummarizeRound_CountsBothKindsOfWorkInTheOneMessage(t *testing.T) {
	// The operator reads the detail, so it has to carry what the round did
	// rather than only that it did something.
	h := newHarness(t, nil)

	h.daemon.summarizeRound(context.Background(), RoundResult{Recovered: 12, GapsFilled: 3})

	events := h.notifier.all()
	if len(events) != 1 {
		t.Fatalf("raised %d notifications, want 1", len(events))
	}
	for _, want := range []string{"12", "3", "broadcasts", "gaps"} {
		if !strings.Contains(events[0].Detail, want) {
			t.Errorf("detail %q does not mention %q", events[0].Detail, want)
		}
	}
}

func TestReportOutcome_RaisesOnlyWhatDoesNotResolveOnItsOwn(t *testing.T) {
	// A recovered broadcast is work that finished and is counted into the
	// summary. A broadcast the round gave up on needs the operator, and
	// nothing else will raise it.
	tests := []struct {
		name string
		kind EventKind
		want bool
	}{
		{name: "gave up", kind: EventFetchGaveUp, want: true},
		{name: "recovered", kind: EventRecovered},
		{name: "gap filled", kind: EventGapFilled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(cfg *config.Config) {
				cfg.Notify.OnFailure = true
				cfg.Notify.OnRecordingStart = true
			})

			h.daemon.reportOutcome(context.Background(), Event{Kind: tt.kind, Channel: "examplechannel"})

			if got := h.notifier.has(tt.kind); got != tt.want {
				t.Errorf("raised %v for %s, want %v", got, tt.kind, tt.want)
			}
		})
	}
}

func TestRunRecovery_TellsTheOperatorOnceForAWholeRound(t *testing.T) {
	// End to end. A round that recovered several broadcasts and gave up on
	// one raises a single summary for the work that finished, and the one
	// outcome that needs the operator on its own.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Notify.OnRecordingStart = true
		cfg.Notify.OnFailure = true
	})
	stub := h.recoverable()
	stub.result = RoundResult{Recovered: 3, GapsFilled: 1}
	stub.announce = []Event{
		{Kind: EventRecovered, Channel: "examplechannel"},
		{Kind: EventRecovered, Channel: "examplechannel"},
		{Kind: EventGapFilled, Channel: "examplechannel"},
		{Kind: EventFetchGaveUp, Channel: "examplechannel", Detail: "the video is private"},
	}
	h.longRecorded()
	h.reachable()

	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if got := h.notifier.count(EventRecovered); got != 1 {
		t.Errorf("raised %d recovered notifications, want one summary for the round", got)
	}
	if got := h.notifier.count(EventGapFilled); got != 0 {
		t.Errorf("raised %d gap notifications, want them counted into the summary", got)
	}
	if got := h.notifier.count(EventFetchGaveUp); got != 1 {
		t.Errorf("raised %d give-up notifications, want the one that needs the operator", got)
	}
}

func TestRunRecovery_StaysSilentAboutACategoryTheOperatorTurnedOff(t *testing.T) {
	// The summary passes the same settings gate every other event does. A
	// category switched off stays off, however much a round recovered.
	h := newHarness(t, func(cfg *config.Config) { cfg.Notify.OnRecordingStart = false })
	stub := h.recoverable()
	stub.result = RoundResult{Recovered: 12, GapsFilled: 3}
	h.longRecorded()
	h.reachable()

	h.daemon.requestRecovery(now.Add(-time.Hour), "an outage", now.Add(-time.Hour))
	h.daemon.runRecovery(context.Background())

	if h.notifier.has(EventRecovered) || h.notifier.has(EventGapFilled) {
		t.Errorf("events = %v, want nothing for a category switched off", h.notifier.kinds())
	}
}

func TestRunRecovery_WarnsAboutATrimmedWindowOnlyWhenOneWasMeasured(t *testing.T) {
	// The routine round asks for as far back as recovery may reach. By the
	// time it runs, the wait has put the request a shade over the horizon,
	// so an unconditional warning fires on every routine round and tells
	// the operator to run a command for a gap they never had. A round held
	// behind a capture can be delayed for hours, so no fixed grace period
	// fixes it: what separates the two is whether anybody measured a gap.
	tests := []struct {
		name string
		// gap is how far back measured coverage stops. Zero is the routine
		// round, which measured nothing and asked for the horizon outright.
		gap  time.Duration
		want bool
	}{
		{name: "the routine round meeting its own horizon"},
		{name: "a gap the margin alone pushed over", gap: recoveryHorizon - time.Hour},
		{name: "a gap that really was longer than the horizon", gap: 90 * 24 * time.Hour, want: true},
		// The clamp lands exactly where coverage stopped, so nothing was
		// lost and nothing is said. This is the row an off-by-one lands
		// on, and the floor is what keeps it from turning into a warning
		// the moment a real clock puts a second between the two.
		{name: "a gap of exactly the horizon", gap: recoveryHorizon},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			stub := h.recoverable()
			h.longRecorded()
			h.reachable()
			messages := &messageLog{}
			h.daemon.logger = slog.New(messages)

			var missing time.Time
			if tt.gap > 0 {
				missing = now.Add(-tt.gap)
			}
			h.daemon.requestRecovery(now.Add(-recoveryHorizon-time.Hour), "a window", missing)
			h.daemon.runRecovery(context.Background())

			if got := messages.mentions("reaches further back"); got != tt.want {
				t.Errorf("warned about a trimmed window = %v, want %v", got, tt.want)
			}
			// The clamp itself is unconditional, and it is the half that
			// stops a month of downtime becoming a month of downloads.
			if round := stub.last(t); !round.Since.Equal(now.Add(-recoveryHorizon)) {
				t.Errorf("round covers from %v, want it clamped to %v",
					round.Since, now.Add(-recoveryHorizon))
			}
		})
	}
}

func TestRunRecovery_SizesATrimmedWindowByTheGapNotByTheRequest(t *testing.T) {
	// The request a long outage produces is clamped to the horizon before
	// it is ever queued, so the margin is all that is left to measure a
	// trim against. Reporting that would size a three-month outage at
	// twelve hours, in the very line telling the operator how far back to
	// reach by hand.
	h := newHarness(t, nil)
	h.recoverable()
	missed := 90 * 24 * time.Hour
	// A library older than the outage, so the whole of it is coverage this
	// recorder could have held and the loss is not bounded by the library's
	// own age instead.
	h.recordedSince(now.Add(-missed - 24*time.Hour))
	h.reachable()
	messages := &messageLog{}
	h.daemon.logger = slog.New(messages)

	h.daemon.requestDowntimeRecovery(context.Background(), Downtime{Since: missed}, now)
	h.daemon.runRecovery(context.Background())

	want := missed - recoveryHorizon
	if got := messages.duration(t, "reaches further back", "trimmed"); got != want {
		t.Errorf("trimmed = %s, want %s", got, want)
	}
}

func TestRunRecovery_NamesNoLossOlderThanTheLibraryItself(t *testing.T) {
	// A restored or rebuilt database reports a library first recorded to
	// moments ago, alongside a previous session whose heartbeat is weeks
	// old. A recorder cannot have missed what aired before it existed, so
	// the stretch before the library is not coverage anybody lost, and
	// naming it sends the operator fetching a window that holds nothing.
	h := newHarness(t, nil)
	h.recoverable()
	h.recordedSince(now.Add(-2 * time.Hour))
	h.reachable()
	messages := &messageLog{}
	h.daemon.logger = slog.New(messages)

	h.daemon.requestDowntimeRecovery(context.Background(),
		Downtime{Since: 30 * 24 * time.Hour}, now)
	h.daemon.runRecovery(context.Background())

	if messages.mentions("reaches further back") {
		t.Error("a loss older than the library was reported, want none named")
	}
}

func TestRunRecovery_StillReportsAGapWhoseWindowTheRoutineRoundSwallowed(t *testing.T) {
	// The routine round asks for the whole horizon, which reaches further
	// back than most outages do, so it wins the coalescing against nearly
	// every measured window. Losing the measurement with the window is a
	// round that trims coverage the recorder watched go missing and tells
	// the operator nothing, which is the one case this warning exists for.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	messages := &messageLog{}
	h.daemon.logger = slog.New(messages)

	// The routine tick first, then a thirteen-day outage whose own window
	// sits inside it. Two days pass with nothing answering a probe, and the
	// clamp lands a day after the outage began.
	missed := 13 * 24 * time.Hour
	h.daemon.requestRecovery(now.Add(-recoveryHorizon), "the routine round", time.Time{})
	h.daemon.requestRecovery(now.Add(-missed-recoveryMargin), "a long outage", now.Add(-missed))
	h.at(now.Add(2 * 24 * time.Hour))
	h.reachable()
	h.daemon.runRecovery(context.Background())

	if !messages.mentions("reaches further back") {
		t.Fatal("a measured gap was trimmed without saying so, want the loss reported")
	}
	if want, got := 24*time.Hour, messages.duration(t, "reaches further back", "trimmed"); got != want {
		t.Errorf("trimmed = %s, want %s", got, want)
	}
	// The reason has to be the outage's, not the tick's. They coalesce
	// separately, so a line pairing the surviving window's reason with the
	// surviving measurement credits thirteen days of loss to a six-hourly
	// tick that measured nothing.
	if got := messages.text(t, "reaches further back", "because"); got != "a long outage" {
		t.Errorf("because = %q, want the reason that measured the gap", got)
	}
	if round := stub.last(t); !round.Since.Equal(h.clockNow().Add(-recoveryHorizon)) {
		t.Errorf("round covers from %v, want it clamped to the horizon", round.Since)
	}
}

func TestRunRecovery_WarnsWhenAHeldWindowIsTrimmedByTheWait(t *testing.T) {
	// A round queued for a measured gap keeps the instant it asked for,
	// while now moves on. Held long enough behind an unanswered probe, the
	// clamp bites after all and coverage the recorder did measure is
	// dropped. Judged on the gap alone that goes unreported, because the
	// gap still fits inside the horizon.
	h := newHarness(t, nil)
	stub := h.recoverable()
	h.longRecorded()
	messages := &messageLog{}
	h.daemon.logger = slog.New(messages)

	// Ten days missed, queued now, and nothing answers a probe for five
	// days afterwards.
	missed := 10 * 24 * time.Hour
	h.daemon.requestRecovery(now.Add(-missed-recoveryMargin), "a long outage", now.Add(-missed))
	h.at(now.Add(5 * 24 * time.Hour))
	h.reachable()
	h.daemon.runRecovery(context.Background())

	if !messages.mentions("reaches further back") {
		t.Error("a held window was trimmed without saying so, want the loss reported")
	}
	// One day of the ten falls outside the horizon by the time the round
	// runs. The margin widened the request by half a day on top, and
	// counting that as lost coverage would send the operator fetching
	// twelve hours nobody missed.
	if want, got := 24*time.Hour, messages.duration(t, "reaches further back", "trimmed"); got != want {
		t.Errorf("trimmed = %s, want %s", got, want)
	}
	// The reason has to be the outage's, not the tick's. They coalesce
	// separately, so a line pairing the surviving window's reason with the
	// surviving measurement credits thirteen days of loss to a six-hourly
	// tick that measured nothing.
	if got := messages.text(t, "reaches further back", "because"); got != "a long outage" {
		t.Errorf("because = %q, want the reason that measured the gap", got)
	}
	if round := stub.last(t); !round.Since.Equal(h.clockNow().Add(-recoveryHorizon)) {
		t.Errorf("round covers from %v, want it clamped to the horizon", round.Since)
	}
}
