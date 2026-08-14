// Package backfill fills the holes the calendar makes visible.
//
// It is the only place that decides a past broadcast is worth fetching.
// The driver in internal/fetch knows how to talk to yt-dlp and the store
// knows how to remember, and neither knows why.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Lister returns a channel's past broadcasts and describes one.
//
// Declared here rather than in internal/fetch, at the point of use, so
// discovery is testable without a subprocess.
type Lister interface {
	List(ctx context.Context, channelURL string) ([]fetch.Listing, error)
	Info(ctx context.Context, url string) (fetch.Listing, error)
}

// Enricher is the optional capability of a Lister that lists and describes
// together.
//
// List plus a lookup per broadcast costs 1 + N, which a channel with years of
// history spends on every pass. A site whose own API answers both at once
// costs one request, so a Lister that can do it is preferred where it exists.
//
// It is asserted for rather than required, the way io.Copy asks whether a
// reader can write itself. Most sites have no such API, and a Lister without
// this is not a lesser one.
type Enricher interface {
	// Describe returns a channel's past broadcasts, described, newest
	// first, reaching back at least to since.
	Describe(ctx context.Context, channelURL string, since time.Time) ([]fetch.Listing, error)
}

// Recorder remembers what discovery found.
type Recorder interface {
	UpsertBroadcast(b store.Broadcast) (store.Broadcast, error)
}

// Discoverer turns a channel's past broadcasts into rows.
type Discoverer struct {
	lister Lister
	store  Recorder
	logger *slog.Logger
}

// Channel is what discovery needs to know about one channel.
type Channel struct {
	// ID is the stored channel.
	ID int64
	// Name is the channel's own name, which the capture stem is built
	// from so a recovered file sorts beside a live one.
	Name string
	// Source names where the listing came from, and keys the scan record
	// so two sources for one channel are rate limited apart.
	Source string
	// URL is the page listing past broadcasts.
	URL string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// liveEndMargin is how close to now a derived end may not land.
//
// A copy the platform is still writing reports a length trailing real
// elapsed time by tens of seconds, so its derived end sits just behind now.
// That is indistinguishable from a broadcast which genuinely just finished,
// and refusing both costs nothing: the settle rule withholds a broadcast for
// far longer than this before anything acts on it, and the next pass records
// the end once it is old enough to be unambiguous.
const liveEndMargin = 15 * time.Minute

// maxDescribed bounds how many listings one pass looks up in detail.
//
// The flat listing is one request, and describing each is one more, so this
// is a ceiling on subprocesses rather than a horizon. The horizon is what
// normally stops the walk: it reaches the oldest day the operator asked for
// and stops there, and this only bounds a channel whose history is deeper
// than the window can be walked in one pass.
const maxDescribed = 100

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// NewDiscoverer returns a discoverer.
func NewDiscoverer(lister Lister, recorder Recorder, logger *slog.Logger) *Discoverer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Discoverer{lister: lister, store: recorder, logger: logger}
}

// ///////////////////////////////////////////////
// Discovery
// ///////////////////////////////////////////////

// Discover lists a channel's past broadcasts and records what it finds.
//
// It reports how many broadcasts it wrote, and lists every time it is
// called rather than caching. A listing is what makes a broadcast that
// ended minutes ago reachable at all, so answering from hours-old rows
// would report nothing found for exactly the broadcast a round was started
// to catch.
func (d *Discoverer) Discover(ctx context.Context, channel Channel, now, since time.Time) (int, error) {
	described, err := d.describe(ctx, channel, since)
	if errors.Is(err, fetch.ErrNoListings) {
		// A channel that has never streamed is not a failure.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("listing %s: %w", channel.Source, err)
	}

	written := 0
	for _, listing := range described {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		if d.record(channel, listing, now) {
			written++
		}
	}

	return written, nil
}

// describe returns a channel's past broadcasts with their start times.
//
// A Lister that can list and describe in one request answers first. Any
// failure there falls back to the two-step path, because enrichment is an
// optimisation and the path it replaces already works: a site API that is
// down, rate limiting, or has revoked a credential must cost a slower
// listing rather than the listing itself.
//
// A cancelled context is the exception. It is the shutdown, and retrying
// through a subprocess would only fail again more slowly.
func (d *Discoverer) describe(ctx context.Context, channel Channel, since time.Time) ([]fetch.Listing, error) {
	enricher, ok := d.lister.(Enricher)
	if !ok {
		return d.listAndDescribe(ctx, channel, since)
	}

	described, err := enricher.Describe(ctx, channel.URL, since)
	switch {
	case err == nil:
		return described, nil
	case ctx.Err() != nil:
		return nil, err
	default:
		d.logger.Debug("could not describe a channel in one request, listing instead",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("error", escape.Field(err.Error())))
		return d.listAndDescribe(ctx, channel, since)
	}
}

// listAndDescribe lists a channel and then looks each broadcast up, newest
// first, back to the horizon.
//
// One request for the listing and one per broadcast after it, which is what
// a site with no API of its own costs. The walk stops at the first broadcast
// that starts before since, because the listing is newest first and
// everything past that one is older still.
//
// A flat listing carries no start times, so the horizon can only be applied
// to what a lookup answers. That costs one request past the bound, which is
// what learning where the bound falls is worth.
func (d *Discoverer) listAndDescribe(ctx context.Context, channel Channel, since time.Time) ([]fetch.Listing, error) {
	listings, err := d.lister.List(ctx, channel.URL)
	if err != nil {
		return nil, err
	}

	described := make([]fetch.Listing, 0, min(len(listings), maxDescribed))
	for _, listing := range listings[:min(len(listings), maxDescribed)] {
		if ctx.Err() != nil {
			return described, ctx.Err()
		}
		if listing.URL == "" {
			continue
		}
		detail, err := d.lister.Info(ctx, listing.URL)
		if err != nil {
			// One broadcast that cannot be described does not end the pass.
			// The others are still worth recording, and the next scan tries
			// this one again.
			d.logger.Debug("could not describe a past broadcast",
				slog.String("id", escape.Field(listing.ID)),
				slog.String("error", escape.Field(err.Error())))
			continue
		}
		described = append(described, detail)

		if !detail.StartedAt.IsZero() && detail.StartedAt.Before(since) {
			break
		}
	}
	return described, nil
}

// record stores one described broadcast, reporting whether it was written.
//
// A listing whose start time is unknown is dropped rather than stored at
// the time of the scan. A broadcast row's start is what the calendar
// buckets by and what a fetch is matched on, and inventing one would put
// a broadcast on the wrong day for good.
func (d *Discoverer) record(channel Channel, described fetch.Listing, now time.Time) bool {
	if described.StartedAt.IsZero() {
		return false
	}

	if _, err := d.store.UpsertBroadcast(store.Broadcast{
		ChannelID:    channel.ID,
		StreamID:     described.StreamID,
		RemoteID:     normalizeID(described.ID),
		URL:          described.URL,
		StartedAt:    described.StartedAt,
		EndedAt:      endOf(described, now),
		VodStartedAt: vodStartOf(described),
		Muted:        mutedOf(described),
		Title:        described.Title,
		Source:       trustOf(described),
		DiscoveredAt: now,
	}); err != nil {
		d.logger.Warn("could not record a past broadcast",
			slog.String("id", escape.Field(described.ID)),
			slog.String("error", escape.Field(err.Error())))
		return false
	}
	return true
}

// normalizeID renders one stored copy's identifier the same way whichever
// source reported it.
//
// Twitch's own API answers 2847353784 and yt-dlp answers v2847353784 for the
// same video, and the remote id is what deduplication matches on. Leaving
// both forms in play mints a prefixed twin of every row already discovered
// the moment one source is unavailable and the other stands in.
//
// The digit run is what makes the rule safe: an identifier that is a bare
// v followed by digits is Twitch's, and a name that merely begins with v is
// left alone.
func normalizeID(id string) string {
	trimmed, found := strings.CutPrefix(id, "v")
	if !found || trimmed == "" {
		return id
	}
	for _, digit := range trimmed {
		if digit < '0' || digit > '9' {
			return id
		}
	}
	return trimmed
}

// endOf turns a listing's length into the moment the broadcast stopped, or
// nil when the length is unreported or the broadcast is still running.
//
// The end is what the settle rule and gap patching both wait for, and a
// listing is the only place backfill can learn one. Nil rather than a guess:
// a broadcast with no recorded end is treated as possibly still running,
// which is the safe reading, and an invented end would file holes against a
// boundary the platform never reported.
//
// A live listing describes a copy the platform is still writing, and reports
// a length that grows between calls. Reading an end from it would place one
// in the past for a broadcast that has not finished, which settles the
// broadcast and releases both the fetch planner and the patcher against a
// copy that is still growing.
//
// Only a source that reports liveness can say so outright, and a platform
// API describing a video need not carry the flag at all. So the derived end
// is refused near now as well, which catches the same copy without needing
// to be told.
func endOf(listing fetch.Listing, now time.Time) *time.Time {
	if listing.IsLive || listing.Duration <= 0 {
		return nil
	}
	ended := listing.StartedAt.Add(listing.Duration)
	if !ended.Before(now.Add(-liveEndMargin)) {
		return nil
	}
	return &ended
}

// vodStartOf returns where the stored copy's own timeline begins, or nil
// when the listing did not report a real timestamp.
//
// It is the same reading the broadcast's start comes from, kept separately
// because the two answer different questions and drift apart: the row's
// start moves as better sources describe the broadcast, while a download
// range is indexed from the moment the platform started recording. An
// imprecise listing carries a date rather than a moment, and a date cannot
// index anything.
func vodStartOf(listing fetch.Listing) *time.Time {
	if !listing.Precise || listing.StartedAt.IsZero() {
		return nil
	}
	started := listing.StartedAt
	return &started
}

// mutedOf carries the platform's answer about what it silenced, keeping an
// unanswered question apart from an answer of nothing.
//
// A listing from a tool with no such field answers nil, and the row stays
// unable to say. Substituting a silence detector here would be worse than
// saying nothing: the platform mutes by replacing the audio with silence, so
// a streamer away from the microphone reads identically, and a false
// positive charges a hole terminal that a real download would have filled.
func mutedOf(listing fetch.Listing) []store.MutedSpan {
	if listing.Muted == nil {
		return nil
	}

	spans := make([]store.MutedSpan, 0, len(listing.Muted))
	for _, span := range listing.Muted {
		spans = append(spans, store.MutedSpan{Offset: span.Offset, Duration: span.Duration})
	}
	return spans
}

// trustOf reports how far a listing's start time may be trusted.
//
// SourceAPI when the platform reported a timestamp, which is a real start
// time. SourceTracker when it reported only a date, because a date says
// nothing about when the broadcast began, and the store's precedence must
// not let one displace a start the recorder watched happen.
func trustOf(listing fetch.Listing) store.Source {
	if listing.Precise {
		return store.SourceAPI
	}
	return store.SourceTracker
}
