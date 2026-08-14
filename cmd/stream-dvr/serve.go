package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/backfill"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/daemon"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/logger"
	"zach.tools/go/stream-dvr/internal/notify"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/providers"
	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/record"
	"zach.tools/go/stream-dvr/internal/secret"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// serve
// ///////////////////////////////////////////////

// eventSink adapts any notify sink to the daemon's event.
//
// The two events carry the same fields and stay separate types on purpose. A
// sink is testable without the daemon, and a rename there is not a change to
// what a notification means. One adapter converts for every sink, so a field
// added to an event reaches all of them together.
type eventSink struct {
	deliver func(context.Context, notify.Event) error
}

// helixLister is the listing tool with Twitch's own API in front of it.
//
// It satisfies backfill.Enricher, which discovery asserts for. Every decision
// lives in the twitch package: whether the address belongs to Twitch, whether
// a session exists, whether the token needs renewing. This is the translation
// between two vocabularies and nothing more.
type helixLister struct {
	fetch.YtDlp
	helix *twitch.Helix
}

// broadcastInfo reports what a download tool knows about one address.
//
// Declared at the point of use, so resolving a start is exercised without
// the tool or a network, and so this depends on no more of the driver than
// it actually reads.
type broadcastInfo interface {
	Info(ctx context.Context, url string) (fetch.Listing, error)
}

// broadcastPlaylist resolves the address a stored copy is served from.
//
// Declared at the point of use for the same reason as broadcastInfo: the
// recovery lookup is exercised without the tool, and it reads nothing else
// from the driver.
type broadcastPlaylist interface {
	Playlist(ctx context.Context, url string) (string, error)
}

// logMaxMegabytes bounds a rotated log file.
const logMaxMegabytes = 10

// startLegTimeout bounds one attempt at resolving a broadcast's start.
//
// Each leg gets its own, so a first one that hangs cannot spend the budget
// the fallback needs. The caller bounds the pair as well, and that outer
// bound is the one protecting the capture.
const startLegTimeout = 4 * time.Second

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "run the recording daemon until interrupted",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "foreground",
				Usage: "also write logs to stderr",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			return runServe(ctx, os.Stdout, configFile(cmd), cmd.Bool("foreground"))
		},
	}
}

// runServe assembles the daemon from configuration and runs it.
//
// Every collaborator is built here and nowhere else, so the wiring is
// visible in one place and the packages below stay unaware of each other's
// construction.
func runServe(ctx context.Context, out io.Writer, configPath string, foreground bool) error {
	out = styled(out)
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		return err
	}

	// The logger is built before the store so the store can be given it. The
	// store's only diagnostic is a row it skipped, which the operator needs
	// and no result set can carry. Left on slog.Default it goes to a stderr
	// the scheduled task does not have.
	log, closeLog, err := buildLogger(lib, foreground)
	if err != nil {
		return err
	}
	defer closeLog.Close()

	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		return err
	}
	db = db.WithLogger(log)
	defer db.Close()

	sinks, closeSinks := buildNotifiers(cfg, lib, log)
	defer closeSinks()

	// Reported from inside the run rather than before it. The claim is the
	// first thing Run takes, and a start refused because another recorder
	// holds the library never reaches this.
	announce := func() {
		reportServeStart(out, lib.Root(), describeWatch(cfg), paths.LogDaemon.Path(lib.StateDir()))
	}

	recorder, _, err := buildDaemon(cfg, lib, db, log, sinks, announce)
	if err != nil {
		return err
	}
	return recorder.Run(ctx)
}

// buildDaemon assembles a recorder and returns it with the organizer behind
// it.
//
// Both callers take the same wiring from here. The organizer comes back
// because the calendar's purge has to go through that one: its per-recording
// lock is the only thing keeping a purge and the sweep's retry of that row
// apart. Two organizers carry two lock tables that know nothing of each other.
// started runs once the library is claimed, for a caller that has something
// to announce. The calendar passes nil: it owns the terminal, so a line
// printed under it would land in the middle of the grid.
func buildDaemon(cfg config.Config, lib *library.Library, db *store.Store,
	log *slog.Logger, notifier daemon.Notifier, started func(),
) (*daemon.Daemon, *organize.Organizer, error) {
	template, err := cfg.Template()
	if err != nil {
		return nil, nil, err
	}
	location, err := cfg.Location()
	if err != nil {
		return nil, nil, err
	}

	// One pipeline drives the remux and the re-encode, so the encoder probe
	// it caches is shared rather than repeated per subsystem.
	pipeline := post.New()
	organizer := organize.New(lib, db, template, pipeline, organize.Options{
		Container: cfg.Capture.Container,
		Location:  location,
	})

	// One metadata client for both readers of it, so the refresh of a
	// one-time-use token stays behind one mutex.
	helix := buildHelix(cfg.Twitch.ClientID)

	recorder, err := daemon.New(daemon.Options{
		Config:         cfg,
		Library:        lib,
		Store:          db,
		Engine:         record.NewStreamlink(record.Options{AuthConfig: authConfig}),
		Finalizer:      organizer,
		Recompressor:   pipeline,
		Credential:     buildCredentialCheck(),
		BroadcastStart: buildBroadcastStart(fetch.New().WithLogger(log), helix, log),
		LiveMetadata:   buildLiveMetadata(helix, log),
		Recover:        automaticRecovery(cfg, lib, db, log, organizer, pipeline, helix, location),
		Started:        started,
		Notifier:       notifier,
		Logger:         log,
	})
	if err != nil {
		return nil, nil, err
	}
	return recorder, organizer, nil
}

// recoveryWindow bounds one automatic round, and reports whether there is a
// window at all.
//
// A window ending before it starts is what a clock stepping backwards
// leaves behind, and it is the one input that must not be passed on. The
// planner reads a lookback of zero or less as unset and substitutes a
// month, so an unguarded one turns a nonsense window into a month of
// downloads nobody asked for.
func recoveryWindow(cfg config.Config, location *time.Location,
	since, now time.Time,
) (backfill.Window, bool) {
	lookback := now.Sub(since)
	if lookback <= 0 {
		return backfill.Window{}, false
	}
	return backfill.Window{
		Lookback: lookback,
		Settle:   cfg.Backfill.Settle.Std(),
		Location: location,
	}, true
}

// onPlatforms keeps the channels whose platform is known to be answering.
//
// A round covers only what it can reach. Every fetch against a platform
// that is down charges an attempt, and a broadcast that spends its last one
// is retired for good, so a channel on an unreachable service is left for
// the round that runs once it answers.
func onPlatforms(channels []backfill.Channel, platforms []string) []backfill.Channel {
	kept := make([]backfill.Channel, 0, len(channels))
	for _, channel := range channels {
		if slices.Contains(platforms, channel.Source) {
			kept = append(kept, channel)
		}
	}
	return kept
}

// automaticRecovery returns the round the recorder runs on its own, or nil
// where the operator has turned that off.
//
// Nil is what the daemon reads as "do not start rounds", so the switch is
// enforced by there being nothing to call rather than by a flag every path
// has to remember to check.
func automaticRecovery(cfg config.Config, lib *library.Library, db *store.Store,
	log *slog.Logger, organizer *organize.Organizer, pipeline *post.Pipeline,
	helix *twitch.Helix, location *time.Location,
) func(context.Context, daemon.Round) (daemon.RoundResult, error) {
	if !cfg.Backfill.Automatic {
		log.Info("automatic recovery is off, so gaps stay until 'stream-dvr backfill' fills them")
		return nil
	}
	return buildRecovery(cfg, lib, db, log, organizer, pipeline, helix, location)
}

// buildRecovery returns the round the recorder runs to fetch what it missed.
//
// The daemon decides when a round runs, because only it knows how long the
// machine was off and which platforms are answering. This is what one does,
// and it is the same engine the backfill command drives.
//
// It takes the recorder's own organizer, pipeline and metadata client, so a
// fetch finalizing a recording and the sweep retrying that same row contend
// for one lock rather than for two that cannot see each other, and so one
// refresh mutex covers every use of a one-time-use token.
//
// The stored channel rows are resolved per round, so a channel first seen
// after the recorder started is planned against rather than skipped.
func buildRecovery(cfg config.Config, lib *library.Library, db *store.Store,
	log *slog.Logger, organizer *organize.Organizer, pipeline *post.Pipeline,
	helix *twitch.Helix, location *time.Location,
) func(context.Context, daemon.Round) (daemon.RoundResult, error) {
	return func(ctx context.Context, round daemon.Round) (daemon.RoundResult, error) {
		resolved, dropped := backfillChannels(cfg, db, log)
		channels := onPlatforms(resolved, round.Platforms)
		// A channel left out because its platform is not answering is work
		// still outstanding, not work found to be unnecessary. Reported so
		// the recorder keeps the window rather than reading a round that
		// covered nothing as one that finished.
		waiting := len(resolved) - len(channels)
		if len(channels) == 0 {
			// Not a failure when nobody opted in. Recovery downloads hours
			// of video from somebody else's service, so backfill = true is
			// the whole permission for it, and a platform nothing can reach
			// is not one to fetch against. A channel the store refused is a
			// different thing, and saying so keeps the window queued.
			return daemon.RoundResult{Failed: dropped, Deferred: waiting}, nil
		}

		now := time.Now()
		window, ok := recoveryWindow(cfg, location, round.Since, now)
		if !ok {
			// The recorder bounds the window before asking, so reaching here
			// means the two disagreed. Counted rather than passed over, so a
			// round that covered nothing cannot read as one that finished.
			return daemon.RoundResult{Failed: 1}, nil
		}

		deps := recoveryEngine(cfg, lib, db, log, organizer, pipeline, helix, round.Session, window)
		deps.Report = func(outcome backfill.Outcome) {
			// Handed back to the recorder rather than pushed at the sinks
			// here. The kinds are the same strings the event kinds are, and
			// the recorder is what reads the operator's notify settings: a
			// round after a long outage recovers broadcasts by the dozen,
			// and a category switched off has to stay off.
			round.Report(daemon.Event{
				Kind:    daemon.EventKind(outcome.Kind),
				Channel: outcome.Channel,
				Title:   outcome.Title,
				Detail:  outcome.Detail,
			})
		}

		// The count comes from the pass rather than from the callback
		// above, because a pass owns the tally of its own work across every
		// goroutine it fetched on.
		result, err := backfill.Pass(ctx, deps, channels, now)
		return daemon.RoundResult{
			Recovered:  result.Recovered,
			GapsFilled: result.GapsFilled,
			Failed:     result.Failed + dropped,
			Deferred:   result.Deferred + waiting,
		}, err
	}
}

// authConfig is the derived streamlink config holding a credential, or ""
// when the operator has not authenticated.
//
// It is a file of its own because streamlink reads a credential from a file
// and from nowhere else: no environment variable, no standard input. The
// interactive auth command writes it, and every capture points streamlink at
// it. A missing one is the ordinary state, not a failure: it records whatever
// is public.
func authConfig() string {
	path := twitch.AuthConfigPath(paths.DataDir())
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// buildCredentialCheck returns the hourly credential check.
//
// It reads the derived file rather than the store, so it checks exactly what
// the recorder presents. A store and a derived file that disagreed would let
// this pass while every capture used a token Twitch had already refused.
//
// It is always returned, because an absent file is not a permanent state:
// the operator runs the auth command afterwards, and the daemon itself
// creates that state whenever it deletes a rejected token. Returning nil for
// it turns the loop off for the life of the process, so a credential stored
// an hour later is never checked again.
//
// A rejection deletes the derived file. A capture presenting a token Twitch
// has already refused gains nothing and costs a round trip on every
// broadcast, so falling back to public quality is the better failure.
func buildCredentialCheck() func(context.Context) error {
	dataDir := paths.DataDir()

	return func(ctx context.Context) error {
		token, err := twitch.ReadAuthConfig(dataDir)
		if err != nil {
			// No credential to check, which is a fresh install, a logout,
			// and what a refusal leaves behind once it deletes the file. The
			// daemon needs those told apart from a token that validates, so
			// this is its own answer rather than success.
			// the absence is the answer, not a failure to get one.
			return daemon.ErrCredentialAbsent
		}

		_, err = twitch.Validate(ctx, nil, token)
		if errors.Is(err, twitch.ErrInvalidToken) {
			if removeErr := twitch.RemoveAuthConfig(dataDir); removeErr != nil {
				return removeErr
			}
			return daemon.ErrCredentialRejected
		}
		return err
	}
}

// Describe implements backfill.Enricher.
func (h helixLister) Describe(ctx context.Context, channelURL string, since time.Time) ([]fetch.Listing, error) {
	videos, err := h.helix.Videos(ctx, channelURL, since)
	if err != nil {
		return nil, err
	}

	listings := make([]fetch.Listing, 0, len(videos))
	for _, video := range videos {
		muted := make([]fetch.MutedSpan, 0, len(video.Muted))
		for _, span := range video.Muted {
			muted = append(muted, fetch.MutedSpan{Offset: span.Offset, Duration: span.Duration})
		}

		listings = append(listings, fetch.Listing{
			ID:        video.ID,
			StreamID:  video.StreamID,
			Title:     video.Title,
			URL:       video.URL,
			StartedAt: video.StartedAt,
			Duration:  video.Duration,
			Muted:     muted,
			// Helix reports a timestamp rather than a date, so a start time
			// from here is exact.
			Precise: true,
		})
	}
	return listings, nil
}

// buildLister returns the tool discovery lists through, with the metadata API
// in front of it when this build and this machine both have one.
//
// A build with no application id, or a machine nobody has authorized, gets
// the tool alone. That is the ordinary case and it works: recovery is slower
// by one request per broadcast and correct either way.
func buildLister(tool fetch.YtDlp, helix *twitch.Helix) backfill.Lister {
	if helix == nil {
		return tool
	}
	return helixLister{YtDlp: tool, helix: helix}
}

// buildHelix returns the metadata client, or nil where this build carries no
// application id.
//
// One client serves every caller. A second one over the same account would
// carry its own refresh mutex, and the invariant that mutex holds is stated
// at twitch.Session: a refresh token is one time use, so two of them
// refreshing at once spend it twice and the survivor is told a live session
// expired.
//
// The daemon reads and writes the same credential file the auth command
// does. It runs as a scheduled task under the operator's own account, so the
// file's mode and the data directory's permissions reach it. A platform
// credential store keyed to an interactive logon is one the recorder cannot
// open, which is why the credential lives in a file.
func buildHelix(clientID string) *twitch.Helix {
	if clientID == "" {
		return nil
	}
	session := twitch.NewSession(clientID, secret.NewFile(paths.DataDir()), secret.AccountTwitchAPI, nil)
	return twitch.NewHelix(clientID, session, nil)
}

// buildBroadcastStart returns the lookup for when a broadcast really began,
// used where the recorder joins a channel that is already on air.
//
// The metadata API goes first. It states the start outright, and it is the
// only source that answers for a channel publishing no archive at all, which
// has nothing for a listing to describe. The download tool answers the rest
// without any credential, reading the copy Twitch starts writing while the
// broadcast is still running.
//
// Neither is required. A machine with no session and a channel with no
// archive simply gets no answer, and the broadcast is anchored to the moment
// the recorder noticed it, which is what it was anchored to regardless.
//
// Both legs answer only about a broadcast running NOW. A finished archive
// satisfies every other test a listing can be put to, so without that gate
// the newest past broadcast is handed back as this one's start.
//
// Each leg gets its own share of the caller's deadline. On one shared budget
// a slow first leg leaves the second none, and the fallback that exists for
// exactly that case never gets to run.
func buildBroadcastStart(tool broadcastInfo, helix *twitch.Helix,
	logger *slog.Logger,
) func(context.Context, string, string) (time.Time, bool) {
	return func(ctx context.Context, channelURL, streamID string) (time.Time, bool) {
		if helix != nil {
			if started, ok := helixStart(ctx, helix, channelURL, streamID, logger); ok {
				return started, true
			}
		}
		return toolStart(ctx, tool, channelURL, streamID, logger)
	}
}

// buildLiveMetadata returns the lookup for what a channel is broadcasting
// now, for a probe that carried no metadata of its own.
//
// The download tool is one source of a title and not the authoritative one.
// A probe that answers with an empty metadata block leaves a capture with no
// title, and a recording with no title is never named into the library: it
// waits in the incoming directory until something supplies one. This is the
// same API that already holds a title for every archived broadcast, asked
// while the broadcast is still on air.
//
// It reports what the platform sees and decides nothing. Whether this is the
// session being captured is the recorder's question, because the recorder
// holds the row to compare against.
//
// A nil client answers false, which leaves the probe's own answer standing.
func buildLiveMetadata(helix *twitch.Helix,
	logger *slog.Logger,
) func(context.Context, string) (daemon.LiveBroadcast, bool) {
	return func(ctx context.Context, channelURL string) (daemon.LiveBroadcast, bool) {
		if helix == nil {
			return daemon.LiveBroadcast{}, false
		}

		stream, live, err := helix.Stream(ctx, channelURL)
		switch {
		case err != nil:
			// Debug for the same reason helixStart is: a machine nobody
			// authorized answers this way on every tick of every capture,
			// and the recovery round supplies the title afterwards.
			logger.DebugContext(ctx, "could not ask the metadata api what is on air",
				slog.String("channel", escape.Field(channelURL)),
				slog.String("error", escape.Field(err.Error())))
			return daemon.LiveBroadcast{}, false
		case !live:
			// The channel went offline between the probe and this call.
			return daemon.LiveBroadcast{}, false
		case stream.Title == "" && stream.Category == "":
			return daemon.LiveBroadcast{}, false
		}
		return daemon.LiveBroadcast{
			StreamID:  stream.ID,
			StartedAt: stream.StartedAt,
			Title:     stream.Title,
			Category:  stream.Category,
		}, true
	}
}

// helixStart asks the metadata API when the current broadcast began.
//
// An answer naming a different live session is refused. A channel that ends
// one broadcast and opens another between the probe and this call would
// otherwise anchor this row to the session that just finished. Twitch is
// authoritative on which session is on air, and it returns the id, so this
// is a comparison rather than a guess.
func helixStart(ctx context.Context, helix *twitch.Helix, channelURL, streamID string,
	logger *slog.Logger,
) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(ctx, startLegTimeout)
	defer cancel()

	stream, live, err := helix.Stream(ctx, channelURL)
	switch {
	case err != nil:
		// Not worth reporting louder: the tool answers the same question a
		// moment later, and a machine nobody authorized would otherwise say
		// so on every broadcast it records.
		logger.DebugContext(ctx, "the metadata api did not answer a broadcast's start",
			slog.String("error", escape.Field(err.Error())))
		return time.Time{}, false
	case !live:
		return time.Time{}, false
	case stream.StartedAt.IsZero():
		// Both legs answer the same question, so both hold the same bar. An
		// answer accepted here without a start is worse than no answer: it
		// stops the tool leg being asked, and that leg may hold a real one.
		return time.Time{}, false
	case streamID != "" && stream.ID != "" && stream.ID != streamID:
		logger.DebugContext(ctx, "the metadata api described a different broadcast",
			slog.String("channel", escape.Field(channelURL)))
		return time.Time{}, false
	}
	return stream.StartedAt, true
}

// toolStart reads the start from the copy the platform writes while the
// broadcast runs.
//
// IsLive is the whole gate. A finished archive reports a precise timestamp
// too, and taking it would anchor this broadcast to a previous one, filing a
// hole covering everything between them.
//
// A live channel address identifies the session rather than a video, so the
// id comes back in the same namespace the probe reported and an answer about
// a different broadcast is refused here as well. Otherwise a session the
// first leg rejected is accepted by the second without the same question
// being asked.
func toolStart(ctx context.Context, tool broadcastInfo, channelURL, streamID string,
	logger *slog.Logger,
) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(ctx, startLegTimeout)
	defer cancel()

	listing, err := tool.Info(ctx, channelURL)
	if err != nil {
		logger.DebugContext(ctx, "the tool did not answer a broadcast's start",
			slog.String("error", escape.Field(err.Error())))
		return time.Time{}, false
	}
	// A date is not a start. Anchoring a broadcast to midnight would file a
	// hole for every hour before the recorder joined.
	if !listing.IsLive || !listing.Precise || listing.StartedAt.IsZero() {
		return time.Time{}, false
	}
	if streamID != "" && listing.ID != "" && listing.ID != streamID {
		logger.DebugContext(ctx, "the tool described a different broadcast",
			slog.String("channel", escape.Field(channelURL)))
		return time.Time{}, false
	}
	return listing.StartedAt, true
}

// buildAudioRecovery returns the lookup for where a copy's silenced
// stretches can be fetched with the audio as broadcast.
//
// It resolves the address the platform serves the copy from, then asks
// whether a route to the original exists beside it. The naming it looks for
// is Twitch's, so an address belonging to anywhere else answers no rather
// than having Twitch's shape imposed on it.
func buildAudioRecovery(tool broadcastPlaylist) func(context.Context, string, []store.MutedSpan) (string, bool, error) {
	twitchAddress := archiveURL(config.Channel{Platform: config.PlatformTwitch, Name: ""})

	return func(ctx context.Context, broadcastURL string, muted []store.MutedSpan) (string, bool, error) {
		if host, ok := hostOf(broadcastURL); !ok || !sameHost(host, twitchAddress) {
			return "", false, nil
		}

		playlist, err := tool.Playlist(ctx, broadcastURL)
		if err != nil {
			return "", false, err
		}

		spans := make([]twitch.Span, 0, len(muted))
		for _, span := range muted {
			spans = append(spans, twitch.Span{Offset: span.Offset, Duration: span.Duration})
		}
		return twitch.OriginalAudio(ctx, nil, playlist, spans)
	}
}

// hostOf returns an address's host.
func hostOf(address string) (string, bool) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	return strings.TrimPrefix(strings.ToLower(parsed.Host), "www."), true
}

// sameHost reports whether an address belongs to the same host as a
// reference address.
func sameHost(host, reference string) bool {
	want, ok := hostOf(reference)
	return ok && host == want
}

// archiveURL is where a channel's past broadcasts are listed.
//
// A platform with no listing answers "", and a channel with no archive is
// skipped rather than searched at an address that cannot answer.
func archiveURL(entry config.Channel) string {
	provider, err := providers.For(entry.Platform)
	if err != nil {
		return ""
	}
	return provider.ArchiveURL(entry.Name)
}

// backfillChannels resolves the channels a pass may act on.
//
// Only channels carrying backfill = true, which is the opt-in for a pass
// that downloads hours of video from somebody else's service.
//
// A channel the recorder has never seen has no stored row and so no coverage
// to have gaps in. It is skipped rather than reported, because that is the
// ordinary state of a channel added to the config a moment ago.
func backfillChannels(cfg config.Config, db *store.Store, log *slog.Logger) ([]backfill.Channel, int) {
	var channels []backfill.Channel
	dropped := 0

	for _, entry := range cfg.EnabledChannels() {
		if !entry.Backfill {
			continue
		}
		// Created if this is the first time the channel is named. Only the
		// recorder's poller made these rows, so a library the daemon has
		// never run against knew no channel at all, and a pass reported
		// that none had backfill turned on while the config plainly said
		// otherwise.
		stored, err := db.UpsertChannel(entry.Platform, entry.Name, "")
		if err != nil {
			// Counted, not just logged. A store briefly unwritable drops
			// every channel, and a round that reported nothing to do would
			// consume the window a restart had just measured.
			dropped++
			log.Warn("could not record a channel to recover",
				slog.String("channel", escape.Field(entry.Name)),
				slog.String("error", escape.Field(err.Error())))
			continue
		}
		channels = append(channels, backfill.Channel{
			ID:     stored.ID,
			Name:   entry.Name,
			Source: entry.Platform,
			URL:    archiveURL(entry),
		})
	}
	return channels, dropped
}

// buildNotifiers assembles every sink the config asks for, and how to stop
// them.
//
// The log is always one of them. It is the sink that cannot be misconfigured
// and cannot be unreachable, so an event lands somewhere readable even when
// the webhook is wrong.
//
// The concrete slice comes back rather than the interface, so a caller with a
// sink of its own appends to it without asserting its way back to one.
func buildNotifiers(cfg config.Config, lib *library.Library, log *slog.Logger) (daemon.Notifiers, func()) {
	sinks := daemon.Notifiers{daemon.LogNotifier{Logger: log}}
	var closers []io.Closer

	if cfg.Notify.Desktop {
		// Reported once, at startup, rather than on every event. A platform
		// that cannot raise one says so where an operator is looking.
		desktop, err := notify.NewDesktop(log)
		switch {
		case errors.Is(err, notify.ErrNeedsAgent):
			// Not a misconfiguration. This platform raises notifications
			// from the agent, which the socket below is what feeds.
			log.Info("desktop notifications go through the notify agent")
		case err != nil:
			log.Warn("desktop notifications are configured but unavailable",
				slog.String("reason", escape.Field(err.Error())))
		default:
			sinks = append(sinks, eventSink{deliver: desktop.Notify})
		}

		// Gated on the same setting, so an operator who turned desktop
		// notifications off is running a recorder with no IPC at all.
		bus, err := notify.Listen(paths.NotifySocket(lib.Root()), log)
		if err != nil {
			log.Warn("could not open the notification socket",
				slog.String("error", escape.Field(err.Error())))
		} else {
			sinks = append(sinks, eventSink{deliver: bus.Notify})
			closers = append(closers, bus)
		}
	}

	if address := strings.TrimSpace(cfg.Notify.WebhookURL); address != "" {
		hook := notify.NewWebhook(address, log)
		sinks = append(sinks, eventSink{deliver: hook.Notify})
		closers = append(closers, hook)
	}

	return sinks, func() {
		for _, closer := range closers {
			if err := closer.Close(); err != nil {
				log.Warn("could not close a notification sink",
					slog.String("error", escape.Field(err.Error())))
			}
		}
	}
}

// Notify implements daemon.Notifier.
//
// The timestamp is stamped once here so that every sink reports one event
// as having happened at one moment.
func (s eventSink) Notify(ctx context.Context, event daemon.Event) error {
	return s.deliver(ctx, notify.Event{
		Kind:    string(event.Kind),
		Channel: event.Channel,
		Title:   event.Title,
		Detail:  event.Detail,
		At:      time.Now(),
	})
}

// buildLogger opens the daemon's rotating log, optionally teeing to stderr.
//
// The log lives inside the library rather than the data directory, because it
// describes that library's recordings. A library moved to another machine
// carries its own history with it.
func buildLogger(lib *library.Library, foreground bool) (*slog.Logger, io.Closer, error) {
	path := paths.LogDaemon.Path(lib.StateDir())

	rotated, closer, err := logger.NewLogger(path, logger.LevelInfo, logMaxMegabytes)
	if err != nil {
		return nil, nil, fmt.Errorf("opening daemon log: %w", err)
	}
	if !foreground {
		return rotated.Logger, closer, nil
	}

	tee := slog.New(slog.NewMultiHandler(
		rotated.Handler(),
		logger.NewHandler(os.Stderr, logger.LevelInfo),
	))
	return tee, closer, nil
}

// reportServeStart says where the recorder files and where its log is.
//
// Every line here carries the library root: logPath embeds it by way of
// StateDir, and watch names channels an operator may have pasted from
// anywhere. A root may legally hold an escape byte on Linux and macOS. Each
// value is escaped before it is styled, never after, because escaping styled
// text would mangle the styling and not the value.
func reportServeStart(out io.Writer, root, watch, logPath string) {
	banner(out, "recording to "+escape.Text(root),
		[]string{escape.Text(watch), "logs: " + escape.Text(logPath)},
		"this runs until you press Ctrl-C")
}

// describeWatch summarizes what the daemon is about to do.
func describeWatch(cfg config.Config) string {
	enabled := cfg.EnabledChannels()
	if len(enabled) == 0 {
		return "no channels enabled; add a [[channels]] block to start recording"
	}

	// A channel name comes from the config file, which is edited by hand and
	// may be carried between machines, so it is no more trustworthy than a
	// stored path.
	names := make([]string, 0, len(enabled))
	for _, channel := range enabled {
		names = append(names, escape.Text(channel.Platform+"/"+channel.Name))
	}
	return fmt.Sprintf("watching %d %s every %s: %s",
		len(names), plural(len(names), "channel", "channels"),
		cfg.Capture.PollInterval.Std().Round(time.Second), joinNames(names))
}

// joinNames renders a channel list, trimming a long one.
func joinNames(names []string) string {
	const shown = 5
	if len(names) <= shown {
		return joinComma(names)
	}
	return fmt.Sprintf("%s and %d more", joinComma(names[:shown]), len(names)-shown)
}

// joinComma joins with commas.
func joinComma(names []string) string {
	var out strings.Builder
	for i, name := range names {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(name)
	}
	return out.String()
}
