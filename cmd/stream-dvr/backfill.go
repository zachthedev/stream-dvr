package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/backfill"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/daemon"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// backfill
// ///////////////////////////////////////////////

// claimBeat is how often a running pass records that it still holds the
// library. It sits well inside daemon.StaleAfter, so a pass that runs for
// hours never looks abandoned to a recorder trying to start.
const claimBeat = 30 * time.Second

func backfillCommand() *cli.Command {
	return &cli.Command{
		Name:  "backfill",
		Usage: "fetch past broadcasts the recorder missed",
		Description: "Fetches the broadcasts a channel has archived and this library does not " +
			"hold, over the range given by --since. The recorder already recovers on its " +
			"own, reaching back two weeks; this is the one-off, for reaching further back " +
			"than that or for not waiting. Only channels with backfill = true are " +
			"considered. It refuses while a " +
			"recorder holds the library, because a second writer would race its sweep. " +
			"--dry-run lists each channel and reports what it would fetch without " +
			"fetching any of it.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "since",
				Usage: "how far back to search, such as 24h or 3d",
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "list what a pass would fetch, and fetch none of it",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			since, err := backfillRange(cmd.String("since"))
			if err != nil {
				return err
			}
			return runBackfill(ctx, os.Stdout, configFile(cmd), since, cmd.Bool("dry-run"))
		},
	}
}

// backfillRange resolves --since.
//
// There is no default. A pass reaches somebody else's service and downloads
// hours of video onto the operator's disk, and a range nobody chose is not
// something to guess at. It takes the duration spellings the config takes,
// so "3d" means one thing across the whole tool.
func backfillRange(text string) (time.Duration, error) {
	if strings.TrimSpace(text) == "" {
		return 0, errors.New("backfill needs a range: pass --since, such as --since 24h or --since 3d")
	}

	parsed, err := config.ParseDuration(text)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("--since must be greater than zero, got %s", escape.Text(text))
	}
	return parsed.Std(), nil
}

// runBackfill fetches what the recorder missed, or reports what it would.
func runBackfill(ctx context.Context, out io.Writer, configPath string,
	since time.Duration, dryRun bool,
) error {
	out = styled(out)
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		return err
	}

	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		return err
	}
	defer db.Close()

	location, err := cfg.Location()
	if err != nil {
		return err
	}
	window := backfill.Window{
		Lookback: since,
		Settle:   cfg.Backfill.Settle.Std(),
		Location: location,
	}

	return runPassOrPlan(ctx, out, cfg, lib, db, window, dryRun)
}

// reportCandidates prints what a pass would fetch, and fetches nothing.
//
// It plans over the channels a pass would run, rather than deriving its own
// set from the config. Two derivations of "which channels are in scope" is
// how a plan comes to describe a pass nobody would get.
func reportCandidates(out io.Writer, db *store.Store, channels []backfill.Channel,
	window backfill.Window, now time.Time,
) error {
	// Every candidate is collected before any is drawn, because the count in
	// each row's first column is a position in a total that is not known
	// until the last channel has been asked.
	found := make([]progress, 0, len(channels))
	scope := make([]row, 0, len(channels))
	for _, channel := range channels {
		candidates, err := backfill.Candidates(db, channel.ID, now, window)
		if err != nil {
			return err
		}

		// Every channel gets a row whether or not it has anything to fetch.
		// A plan that listed only broadcasts would say nothing at all about
		// a channel it looked at and found complete, which is the answer an
		// operator most often runs this to get.
		state := outcomeNote
		if len(candidates) > 0 {
			state = outcomePass
		}
		scope = append(scope, row{
			State:   state,
			Label:   escape.Text(channel.Source + "/" + channel.Name),
			Trailer: fmt.Sprintf("%d to fetch", len(candidates)),
		})

		for _, candidate := range candidates {
			found = append(found, progress{
				State:   outcomePass,
				Subject: escape.Text(channel.Source + "/" + channel.Name),
				When: candidate.Broadcast.StartedAt.In(window.Location).
					Format("2006-01-02 15:04"),
				Detail: escape.Text(candidate.Broadcast.Title),
			})
		}
	}
	for i := range found {
		found[i].Index, found[i].Total = i+1, len(found)
	}

	section(out, "channels", scope)
	steps(out, "would fetch", found)
	next := "run 'stream-dvr backfill' without --dry-run to fetch them"
	if len(found) == 0 {
		next = ""
	}
	summary(out, fmt.Sprintf("%d to fetch across %d %s", len(found), len(channels),
		plural(len(channels), "channel", "channels")), next)
	return nil
}

// runPassOrPlan lists what each channel has archived, then either fetches
// what is missing or reports what fetching would do.
//
// Both paths hold the library. A plan is not a read: it lists a channel from
// the platform and stores what it learns, because a plan computed from rows
// alone reports nothing to fetch on any library nothing has listed yet,
// which is every library before the first pass.
//
// The claim is the same row a recorder takes, so a pass and a daemon cannot
// both be writing. Reading whether one runs and then acting leaves a window
// where a recorder starts in between, and StartSession settles it in one
// transaction with the write lock already held.
func runPassOrPlan(ctx context.Context, out io.Writer, cfg config.Config,
	lib *library.Library, db *store.Store, window backfill.Window, dryRun bool,
) (err error) {
	session, err := db.StartSession(time.Now(), daemon.StaleAfter)
	if errors.Is(err, store.ErrRecorderRunning) {
		return errors.New("a recorder holds this library; stop it first, " +
			"because a second writer would race its sweep")
	}
	if err != nil {
		return err
	}
	defer func() {
		// Joined to the pass's own error rather than dropped. A claim left
		// open blocks every later run until it goes stale, and an operator
		// who is not told reads that as the command being broken.
		if stopErr := db.StopSession(session.ID, time.Now()); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("releasing the library claim: %w", stopErr))
		}
	}()

	log, closeLog, err := buildLogger(lib, true)
	if err != nil {
		return err
	}
	defer closeLog.Close()
	db = db.WithLogger(log)

	// Through the same sinks a capture reports to. A pass over a month of
	// history runs for hours, and an operator who started one and walked
	// away is exactly who the desktop notification was for.
	sinks, closeSinks := buildNotifiers(cfg, lib, log)
	defer closeSinks()

	// The beat stops before the claim is released, so nothing writes a
	// heartbeat onto a session this call has already closed.
	beating, stopBeating := context.WithCancel(ctx)
	var beats sync.WaitGroup
	beats.Go(func() { holdClaim(beating, db, session.ID, log) })
	defer func() {
		stopBeating()
		beats.Wait()
	}()

	channels, _ := backfillChannels(cfg, db, log)
	if len(channels) == 0 {
		fmt.Fprintf(out, "%s\n", styleDim.Render(
			"no channel has backfill = true, so there is nothing to fetch"))
		return nil
	}

	fmt.Fprintf(out, "%s %s\n", styleDim.Render(glyphNote), styleDim.Render(fmt.Sprintf(
		"searching %s back across %d %s", window.Lookback, len(channels),
		plural(len(channels), "channel", "channels"))))

	if dryRun {
		discoverer := backfill.NewDiscoverer(buildLister(fetch.New().WithLogger(log), buildHelix(cfg.Twitch.ClientID)), db, log)
		if err := listArchives(ctx, discoverer, log, window, channels); err != nil {
			return err
		}
		return reportCandidates(out, db, channels, window, time.Now())
	}
	return runPass(ctx, out, cfg, lib, db, log, sinks, session.ID, window, channels)
}

// listArchives records what each channel has archived, so a plan is computed
// against what the platform holds rather than against what this library
// happens to have heard of.
func listArchives(ctx context.Context, discovery backfill.Discovery, log *slog.Logger,
	window backfill.Window, channels []backfill.Channel,
) error {
	for _, channel := range channels {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := discovery.Discover(ctx, channel, time.Now(),
			time.Now().Add(-window.Lookback)); err != nil {
			// One channel's listing failing leaves the others worth
			// planning, the same way a pass carries on past one of them.
			log.Warn("could not list past broadcasts",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("error", escape.Field(err.Error())))
		}
	}
	return nil
}

// holdClaim records that this pass still holds the library until it ends.
func holdClaim(ctx context.Context, db *store.Store, session int64, log *slog.Logger) {
	ticker := time.NewTicker(claimBeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := db.Heartbeat(session, time.Now()); err != nil {
				log.Warn("could not record that the pass still holds the library",
					slog.String("error", escape.Field(err.Error())))
			}
		}
	}
}

// recoveryEngine assembles the collaborators one recovery round needs.
//
// The backfill command and the recorder's own recovery loop both take their
// wiring from here, so the two cannot drift into fetching under different
// rules. Report is left unset: a round does the same work either way, and
// where it says what it did is the only thing the two callers disagree
// about.
//
// The organizer, the pipeline and the metadata client are passed in rather
// than built here. The organizer's per-recording lock is what holds a
// fetch's finalize and the sweep's retry of that same row apart, and a
// second organizer carries a second lock table that knows nothing of the
// first. The pipeline caches what this machine can encode with, which is
// worth probing once. The metadata client holds the refresh mutex, and a
// refresh token is one time use: two clients over one account spend it
// twice and the loser is told a live session expired.
//
// Discovery is wired on every round. A listing is what makes a broadcast
// that ended minutes ago reachable at all, so a round that planned from the
// database alone would report finding nothing and be right about the wrong
// question.
func recoveryEngine(cfg config.Config, lib *library.Library, db *store.Store,
	log *slog.Logger, organizer *organize.Organizer, pipeline *post.Pipeline,
	helix *twitch.Helix, session int64, window backfill.Window,
) backfill.PassDeps {
	finalize := func(c context.Context, id int64) error {
		_, err := organizer.Finalize(c, id)
		return err
	}

	tool := fetch.New().WithLogger(log)
	admit := admitDownload(cfg, lib, db)

	return backfill.PassDeps{
		Coverage: db,
		Discover: backfill.NewDiscoverer(buildLister(tool, helix), db, log),
		Fetch: backfill.NewFetcher(tool, db, finalize, pipeline, lib.Root(), session,
			backfill.FetchOptions{
				RateLimit:     cfg.Backfill.RateLimit,
				Admit:         admit,
				OriginalAudio: buildAudioRecovery(tool),
			}, log),
		Patch: backfill.NewPatcher(tool, db, finalize, pipeline, backfill.PatchOptions{
			LibraryRoot:   lib.Root(),
			RateLimit:     cfg.Backfill.RateLimit,
			MaxAttempts:   cfg.Backfill.MaxAttempts,
			Admit:         admit,
			OriginalAudio: buildAudioRecovery(tool),
		}, log),
		Window:        window,
		MaxConcurrent: cfg.Backfill.MaxConcurrent,
		Logger:        log,
	}
}

// runPass runs one round and draws what it did.
//
// The organizer is built here because this command owns the only one in the
// process: nothing else is running against the library, which the claim
// taken above is what guarantees.
func runPass(ctx context.Context, out io.Writer, cfg config.Config, lib *library.Library,
	db *store.Store, log *slog.Logger, notifier daemon.Notifier, session int64,
	window backfill.Window, channels []backfill.Channel,
) error {
	template, err := cfg.Template()
	if err != nil {
		return err
	}

	pipeline := post.New()
	organizer := organize.New(lib, db, template, pipeline, organize.Options{
		Container: cfg.Capture.Container,
		Location:  window.Location,
	})
	// This command holds the only claim on the library, so nothing else in
	// the process is refreshing the same one-time-use token.
	deps := recoveryEngine(cfg, lib, db, log, organizer, pipeline,
		buildHelix(cfg.Twitch.ClientID), session, window)

	// The run is opened before the pass rather than after it, so the first
	// row lands as soon as the first broadcast does. Its length is not known
	// in advance: discovery lists the platform first and may find broadcasts
	// no candidate query had seen, so the rows count up without a total to
	// count towards.
	names := make([]string, 0, len(channels))
	for _, channel := range channels {
		names = append(names, escape.Text(channel.Source+"/"+channel.Name))
	}
	run := newRunner(out, "fetching", names, 0)

	deps.Report = func(outcome backfill.Outcome) {
		reportOutcome(run, outcome)

		if err := notifier.Notify(ctx, daemon.Event{
			Kind:    daemon.EventKind(outcome.Kind),
			Channel: outcome.Channel,
			Title:   outcome.Title,
			Detail:  outcome.Detail,
		}); err != nil {
			log.Warn("could not report a backfill outcome",
				slog.String("error", escape.Field(err.Error())))
		}
	}

	// Counted by the pass rather than by this callback. The counters are
	// what the summary line reports, and a pass owns the tally of its own
	// work across every goroutine it fetched on.
	result, passErr := backfill.Pass(ctx, deps, channels, time.Now())
	if passErr != nil {
		return passErr
	}

	// No next action. A pass that fetched what it could has nothing left for
	// the operator to type, and one that gave up said why on the row it gave
	// up on.
	summary(out, fmt.Sprintf("%d fetched, %d %s filled",
		result.Recovered, result.GapsFilled,
		plural(result.GapsFilled, "gap", "gaps")), "")
	return nil
}

// reportOutcome prints one thing a pass did.
//
// Every field came from a platform listing or a subprocess, so each is
// escaped before it is styled, never after.
func reportOutcome(run *runner, outcome backfill.Outcome) {
	state := outcomePass
	if outcome.Kind == backfill.OutcomeGaveUp {
		state = outcomeFail
	}

	detail := outcome.Detail
	if outcome.Title != "" {
		detail = trailer(outcome.Title, detail)
	}
	run.step(state, escape.Text(outcome.Channel), escape.Text(detail))
}

// admitDownload answers whether the library has room for a fetch.
//
// It asks the question a capture is admitted against, through the same
// limits, so a download and a recording cannot disagree about how full the
// library is. A fetch writes into the incoming directory, and those bytes
// are invisible to the size cap until the file is claimed, so they are
// counted here rather than left out.
//
// The recorder uses this too, and a capture may be writing while it does.
// Admission is judged once, before a download starts, so what actually
// keeps the two apart is that a round does not run beside a capture and
// stands aside for one that begins.
func admitDownload(cfg config.Config, lib *library.Library, db *store.Store) func(int64) error {
	limits := space.Limits{MaxSize: cfg.Space.MaxSize, MinFree: cfg.Space.MinFree}

	return func(estimate int64) error {
		stored, err := db.TotalBytes()
		if err != nil {
			return err
		}
		incoming, err := incomingBytes(lib.IncomingDir())
		if err != nil {
			return err
		}
		free, err := space.Free(lib.Root())
		if err != nil {
			return err
		}
		return space.Admit(limits,
			space.Usage{LibraryBytes: stored + incoming, FreeBytes: free}, estimate)
	}
}

// incomingBytes sums what the incoming directory holds.
//
// Every byte there counts against the budget, including a capture in
// progress: the recorder calls this too, and a file already claimed is
// counted twice at worst. Over-counting refuses a fetch that would have
// fitted; under-counting fills the disk the budget exists to guard.
func incomingBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		// No capture and no fetch has ever run against this library.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", dir, err)
	}

	total := int64(0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			// Claimed and moved into the library while this was reading, so
			// its bytes are in the stored total instead.
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("sizing %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		total += info.Size()
	}
	return total, nil
}
