// Package config defines stream-dvr's configuration, its defaults, and the
// validation every field must pass before the daemon starts.
//
// Validation is strict and happens once, at load. A template typo or an
// unparseable size is a startup failure, not a surprise at the moment a
// broadcast ends. Nothing here reads the environment or the filesystem
// beyond the config file itself, so the same Config can be constructed in
// a test.
package config

import (
	"fmt"
	"strings"

	"zach.tools/go/stream-dvr/internal/generate"
	"zach.tools/go/stream-dvr/internal/naming"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Config is the complete configuration.
//
// The JSON tags drive the published schema, not the TOML decoder. Loading
// merges a file over DefaultConfig, so a partial config is the normal case.
// omitempty marks every key a file can leave out, and what stays required
// is what has no default. Size and Duration carry an explicit jsonschema
// type because they are written as text and reflect as int64.
//
// omitempty on a struct-typed field here is load-bearing, and does not look
// it. encoding/json ignores it, because a struct is never empty, so a tool
// reading only that will offer to delete it as dead weight. The schema
// generator reads the same tag to decide which keys are required, and
// dropping it makes every section mandatory: an editor then reports a config
// that omits [space] as invalid, when omitting it is how an operator asks for
// the defaults.
type Config struct {
	// Version is the config format version. It exists from the first
	// release so a later format change has something to migrate from.
	Version int     `toml:"schema_version" json:"schema_version,omitempty"`
	Library Library `toml:"library" json:"library"`
	Space   Space   `toml:"space" json:"space,omitempty"`
	Capture Capture `toml:"capture" json:"capture,omitempty"`
	Naming  Naming  `toml:"naming" json:"naming,omitempty"`
	Notify  Notify  `toml:"notify" json:"notify,omitempty"`

	// Backfill holds how a recovery pass behaves, whether the recorder
	// started it or the operator did. Only a channel carrying Backfill is
	// ever fetched for.
	Backfill Backfill `toml:"backfill" json:"backfill,omitempty"`
	// Twitch holds the application this install authenticates as. Empty
	// leaves the metadata API unreachable, which costs broadcast titles
	// and the batched listing a recovery pass reads.
	Twitch Twitch `toml:"twitch" json:"twitch,omitempty"`
	// omitempty keeps "channels = []" out of a generated config. TOML
	// refuses to let an empty array and a [[channels]] table array share a
	// key, so emitting it would break the first channel an operator adds.
	Channels []Channel `toml:"channels,omitempty" json:"channels,omitempty"`
}

// Library locates the recording library. It has one job: where recordings
// live. What happens as that directory fills is Space.
type Library struct {
	// Root is the library directory. It must already be a library, which
	// "stream-dvr library init" or "library adopt" creates.
	Root string `toml:"root" json:"root"`
}

// Space is the disk budget and the two mechanisms that answer a full
// library, held in the order they cost the operator.
//
// Recompress changes how much space a recording takes. Purge changes how
// many recordings there are, and refusing the next broadcast is where the
// ladder falls to when neither runs. Only the operator crosses from the
// first to the second: nothing here deletes a recording on its own.
type Space struct {
	// MaxSize caps the total bytes of recordings. Zero means no cap.
	MaxSize Size `toml:"max_size" json:"max_size,omitempty" jsonschema:"type=string"`
	// MinFree is the free space that must remain on the volume. It guards
	// the disk against the DVR when other things share it.
	MinFree Size `toml:"min_free" json:"min_free,omitempty" jsonschema:"type=string"`
	// Recompress re-encodes older recordings, spending picture quality to
	// free space.
	Recompress Recompress `toml:"recompress" json:"recompress,omitempty"`
	// Purge scores recordings for the assisted purge, which spends whole
	// broadcasts and keeps the quality of what remains.
	Purge Purge `toml:"purge" json:"purge,omitempty"`
}

// Capture controls how broadcasts are recorded.
type Capture struct {
	// PollInterval is how often a channel is checked for a live
	// broadcast.
	PollInterval Duration `toml:"poll_interval" json:"poll_interval,omitempty" jsonschema:"type=string"`
	// Quality is the streamlink quality ladder, tried in order.
	Quality []string `toml:"quality" json:"quality,omitempty"`
	// MinDuration is the length a finished recording must reach to be
	// named into the library. A shorter one is marked failed, which keeps
	// accidental restarts and connection tests out of the library. The
	// file stays on disk: the daemon never deletes a recording.
	MinDuration Duration `toml:"min_duration" json:"min_duration,omitempty" jsonschema:"type=string"`
	// MaxConcurrent bounds simultaneous recordings, since each one costs
	// bandwidth and disk throughput.
	MaxConcurrent int `toml:"max_concurrent" json:"max_concurrent,omitempty"`
	// Container is the extension finished recordings are remuxed into.
	Container string `toml:"container" json:"container,omitempty"`
}

// Naming controls how a finished recording is named.
type Naming struct {
	// Template is the path template. See the naming package for the
	// placeholder set.
	Template string `toml:"template" json:"template,omitempty"`
	// Timezone names the location dates render in. "Local" uses the
	// machine's zone; anything else must be an IANA name.
	Timezone string `toml:"timezone" json:"timezone,omitempty"`
}

// Twitch holds the Twitch application this install authenticates as.
type Twitch struct {
	// ClientID is the Twitch application this install acts as, registered
	// by the operator at dev.twitch.tv/console/apps.
	//
	// There is no shipped default, and that is deliberate. One baked into
	// the binary would make every download act as the same registration,
	// which names whoever produced the build as the developer answerable
	// for what any of them does, and puts one application's standing in
	// the hands of strangers. A client id is public rather than secret, so
	// keeping it here costs nothing to protect.
	ClientID string `toml:"client_id" json:"client_id,omitempty"`
}

// Backfill configures the recovery pass, which fetches past broadcasts the
// recorder did not capture.
//
// Every field here shapes a pass whoever started it. The recorder starts
// one after a restart, after an outage ends, and every few hours as a
// matter of course, reaching back a fortnight at most. The backfill command
// starts one over any range the operator names. Reaching somebody else's
// service for hours of video needs permission either way, and Channel's own
// Backfill field is that permission.
//
// A recovered copy is worth less than a live one: platforms mute a stored
// copy after the fact. So this fills holes rather than replacing anything,
// and never touches a broadcast the recorder already has.
type Backfill struct {
	// Automatic lets the recorder start rounds of its own: after a
	// restart, after an outage ends, and every few hours as a matter of
	// course. Off leaves the backfill command as the only thing that
	// fetches, which is a recorder that keeps recording and stops filling
	// its own gaps.
	Automatic bool `toml:"automatic" json:"automatic,omitempty"`
	// Settle is how long to leave a broadcast alone after it ends. A copy
	// is not published the instant a stream stops.
	Settle Duration `toml:"settle" json:"settle,omitempty" jsonschema:"type=string"`
	// MaxConcurrent bounds simultaneous fetches. One by default, because
	// a fetch competes with recording for the same link and disk.
	MaxConcurrent int `toml:"max_concurrent" json:"max_concurrent,omitempty"`
	// MaxAttempts bounds how many times one hole inside a captured
	// broadcast is patched before it is left alone.
	//
	// A whole broadcast is not capped this way. Retiring one is permanent
	// and no command undoes it, so a failed fetch waits longer each time
	// instead, and the window recovery reaches back is what lets it go.
	MaxAttempts int `toml:"max_attempts" json:"max_attempts,omitempty"`
	// RateLimit caps a fetch's bandwidth, in the form yt-dlp takes, such
	// as "5M". Empty leaves it uncapped.
	RateLimit string `toml:"rate_limit" json:"rate_limit,omitempty"`
}

// Recompress re-encodes older recordings to a denser codec.
//
// It costs picture quality and it costs no broadcasts, which makes it the
// last step that frees space without losing a recording. The loss is
// permanent: the video is decoded and compressed again, and the original
// bitstream is gone unless KeepOriginal is set. Remuxing is a stream copy
// and saves the few percent that MPEG-TS overhead costs, so a real
// reduction needs this. After holds the stage off recent recordings, so
// nothing is degraded before there is a reason to reclaim its space.
type Recompress struct {
	// Enabled turns the whole stage on. Off means recordings keep their
	// original bitstream forever.
	Enabled bool `toml:"enabled" json:"enabled,omitempty"`
	// After is how old a recording must be before it is re-encoded.
	After Duration `toml:"after" json:"after,omitempty" jsonschema:"type=string"`
	// Codec is the target, one of RecompressCodecs.
	Codec string `toml:"codec" json:"codec,omitempty"`
	// Quality is the encoder's constant-quality level, and it is the
	// quality being spent. Lower is better and larger; the scale matches
	// CRF and CQ, where 30 suits stream content.
	Quality int `toml:"quality" json:"quality,omitempty"`
	// PreferHardware uses a GPU encoder when one is present. Hardware is
	// far faster and somewhat larger at the same perceived quality.
	PreferHardware bool `toml:"prefer_hardware" json:"prefer_hardware,omitempty"`
	// MaxConcurrent bounds simultaneous re-encodes. Encoding competes
	// with recording for the same disk and CPU, so this stays low.
	MaxConcurrent int `toml:"max_concurrent" json:"max_concurrent,omitempty"`
	// KeepOriginal retains the source after a verified re-encode. It is
	// what makes the quality loss reversible, and it spends the entire
	// space saving to do it.
	KeepOriginal bool `toml:"keep_original" json:"keep_original,omitempty"`
}

// Purge scores recordings for the assisted purge. Nothing here causes a
// deletion: the daemon only ranks, and the operator decides.
type Purge struct {
	// WatchedWeight raises the purge score of a recording already
	// watched.
	WatchedWeight float64 `toml:"watched_weight" json:"watched_weight,omitempty"`
	// AgeWeight raises the purge score as a recording ages, applied per
	// full week.
	AgeWeight float64 `toml:"age_weight" json:"age_weight,omitempty"`
	// RefetchableWeight raises the purge score of a recording whose
	// broadcast still exists upstream, because deleting it is reversible.
	RefetchableWeight float64 `toml:"refetchable_weight" json:"refetchable_weight,omitempty"`
	// ProtectFor keeps a recording off the purge list entirely until it
	// reaches this age.
	ProtectFor Duration `toml:"protect_for" json:"protect_for,omitempty" jsonschema:"type=string"`
	// TrashGrace is how long a purged recording stays recoverable before
	// its bytes are released.
	TrashGrace Duration `toml:"trash_grace" json:"trash_grace,omitempty" jsonschema:"type=string"`
}

// Notify configures where alerts go.
type Notify struct {
	// Desktop enables a native desktop notification.
	Desktop bool `toml:"desktop" json:"desktop,omitempty"`
	// WebhookURL receives a JSON payload per event. Empty disables it.
	WebhookURL string `toml:"webhook_url" json:"webhook_url,omitempty"`
	// OnRecordingStart fires when a capture begins.
	OnRecordingStart bool `toml:"on_recording_start" json:"on_recording_start,omitempty"`
	// OnFailure fires when a capture or post-processing step fails.
	OnFailure bool `toml:"on_failure" json:"on_failure,omitempty"`
	// OnLibraryFull fires when the cap stops a recording, which is the
	// one event that costs a broadcast.
	OnLibraryFull bool `toml:"on_library_full" json:"on_library_full,omitempty"`
}

// Channel is one watched source.
type Channel struct {
	// Platform selects the integration. See SupportedPlatforms.
	Platform string `toml:"platform" json:"platform"`
	// Name is the channel login for a platform, or the full URL when the
	// platform is "url".
	Name string `toml:"name" json:"name"`
	// Enabled allows a channel to be kept in config but not watched.
	Enabled bool `toml:"enabled" json:"enabled,omitempty"`
	// Quality overrides Capture.Quality for this channel. Empty inherits.
	Quality []string `toml:"quality" json:"quality,omitempty"`
	// Backfill allows the recovery engine to fetch this channel's missed
	// broadcasts from public archives.
	Backfill bool `toml:"backfill" json:"backfill,omitempty"`
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// SchemaVersion is the config format this build writes and understands.
// A file claiming a higher version was written by a newer binary and is
// refused rather than parsed with fields silently dropped.
const SchemaVersion = 1

// Platform identifiers accepted in a channel's platform field.
const (
	// PlatformTwitch has full support: metadata, VOD recovery, and
	// tracker-based gap detection.
	PlatformTwitch = "twitch"
	// PlatformYouTube supports live capture and backfill of past
	// broadcasts.
	PlatformYouTube = "youtube"
	// PlatformURL captures any streamlink-supported URL with minimal
	// metadata and no recovery.
	PlatformURL = "url"
)

// Recompress target codecs.
const (
	// CodecAV1 gives the largest reduction. Hardware encoding needs a
	// recent GPU and no Mac has one. Software encoding takes hours per
	// broadcast.
	CodecAV1 = "av1"
	// CodecHEVC reduces less but encodes on hardware far more machines
	// have and plays on more devices, which is why it is the default.
	CodecHEVC = "hevc"
)

// Quality bounds for a constant-quality encode.
const (
	// MinQuality is the highest-fidelity setting worth offering.
	MinQuality = 1
	// MaxQuality is the point past which artifacts dominate.
	MaxQuality = 63
)

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

// SupportedPlatforms lists every accepted platform, in the order doctor
// and error messages present them.
var SupportedPlatforms = []string{PlatformTwitch, PlatformYouTube, PlatformURL}

// RecompressCodecs lists every accepted re-encode target.
var RecompressCodecs = []string{CodecAV1, CodecHEVC}

// ///////////////////////////////////////////////
// Documentation
// ///////////////////////////////////////////////

// Docs maps dotted config paths to the comments written above each field in
// the generated config. It is the single source of truth for field
// documentation, so the generated file and the JSON schema cannot drift.
var Docs = map[string]generate.FieldDoc{
	"schema_version": {
		Comment: "Config format version. Leave it alone; stream-dvr uses it to\n" +
			"migrate this file when the format changes.",
	},

	"library": {Comment: "Where recordings live."},
	"library.root": {
		Comment: "Library directory, which 'stream-dvr library init <path>' creates\n" +
			"and writes here. 'library adopt <path>' does the same for a folder\n" +
			"of recordings you already have. Pass --force to either to repoint\n" +
			"this at another library.",
		Alternatives: []string{`root = "/srv/vods"`},
	},

	"space": {
		Comment: "What happens as the library fills. These settings are ordered by what\n" +
			"they cost you, cheapest first, and that is the order they apply in.\n" +
			"Everything down to recompress changes how much space a recording\n" +
			"takes. Purging and refusing change how many recordings you have. Only\n" +
			"you cross that line: nothing here deletes a recording on its own.",
	},
	"space.max_size": {
		Comment: "Cap on total recording bytes. 0 disables the cap. Reaching it refuses\n" +
			"the next broadcast, which is the one outcome that costs you something\n" +
			"unrecorded, so leave room for the steps below to work in.\n" +
			"KB is 1000 bytes and KiB is 1024; both are accepted.",
	},
	"space.min_free": {
		Comment: "Free space that must remain on the volume for everything that is not\n" +
			"this library. Recording stops before this is consumed, even if\n" +
			"max_size still has room.",
	},

	"space.recompress": {
		Comment: "Re-encode older recordings to a denser codec. COSTS PICTURE QUALITY,\n" +
			"permanently: the video is decoded and compressed again, and the\n" +
			"original bitstream is gone unless keep_original is set. Costs no\n" +
			"broadcasts. It is the last step that frees space without losing a\n" +
			"recording.",
	},
	"backfill": {
		Comment: "How a recovery pass behaves, whether the recorder started it or\n" +
			"you did. The recorder recovers on its own: after a restart, after\n" +
			"an outage ends, and every six hours as a matter of course,\n" +
			"reaching back at most two weeks. Only channels with backfill =\n" +
			"true are ever fetched. 'stream-dvr backfill --since 24h' is the\n" +
			"one-off, for reaching further back than that or for not waiting.\n" +
			"A fetched copy is worth less than a live recording, because\n" +
			"platforms mute a stored copy after the fact, so it never\n" +
			"replaces one you have.",
	},
	"backfill.automatic": {
		Comment: "Let the recorder fetch what it missed without being asked: after\n" +
			"a restart, after an outage ends, and every few hours. Off leaves\n" +
			"'stream-dvr backfill' as the only thing that fetches, so gaps\n" +
			"stay until you fill them. Either way a channel is fetched for\n" +
			"only when its own backfill setting is on.",
	},
	"backfill.settle": {
		Comment: "How long to leave a broadcast alone after it ends. A copy is not\n" +
			"published the instant a stream stops, and a broadcast with no\n" +
			"recorded end may still be running.",
	},
	"backfill.max_concurrent": {
		Comment: "How many fetches run at once. A fetch competes with recording for\n" +
			"the same link and disk, so this stays low.",
	},
	"backfill.max_attempts": {
		Comment: "How many times one broadcast is retried before it is left alone.\n" +
			"Without a cap, a broadcast failing for a reason nothing recognizes\n" +
			"is retried on every pass forever.",
	},
	"backfill.rate_limit": {
		Comment: "Caps a fetch's bandwidth, in the form yt-dlp takes, such as 5M.\n" +
			"Empty leaves it uncapped, which can saturate the link during a live\n" +
			"capture.",
	},
	"space.recompress.enabled": {
		Comment: "Off by default, because the loss cannot be undone, and because a\n" +
			"machine with no hardware encoder falls back to software at hours per\n" +
			"broadcast. Run 'stream-dvr doctor' to see the encoder this machine\n" +
			"would actually use before turning it on.",
	},
	"space.recompress.after": {
		Comment: "How old a recording must be before it is re-encoded. A shorter age\n" +
			"degrades recordings you may still be watching.",
	},
	"space.recompress.codec": {
		Comment: fmt.Sprintf("One of: %s. Costs encoding time, not quality. av1 is smaller;\n"+
			"hevc encodes on hardware far more machines have and plays on more\n"+
			"devices.", strings.Join(RecompressCodecs, ", ")),
	},
	"space.recompress.quality": {
		Comment: "Constant-quality level, 1 to 63. This is the quality you are\n" +
			"spending. Lower is better and larger. 30 suits stream content.",
	},
	"space.recompress.prefer_hardware": {
		Comment: "Use a GPU encoder when one is present. Costs a little size at the\n" +
			"same perceived quality and saves hours per recording.",
	},
	"space.recompress.max_concurrent": {
		Comment: "Simultaneous re-encodes. Costs disk and CPU the recorder needs, so\n" +
			"keep this low.",
	},
	"space.recompress.keep_original": {
		Comment: "Keep the source after a verified re-encode. This is what makes the\n" +
			"quality loss reversible, and it spends the entire space saving to do\n" +
			"it, so the pair only makes sense while you judge the result.",
	},

	"space.purge": {
		Comment: "Scoring for the assisted purge. COSTS WHOLE BROADCASTS, and keeps the\n" +
			"quality of everything that remains. Nothing here deletes anything:\n" +
			"the daemon ranks candidates and you confirm, every time.",
	},
	"space.purge.watched_weight": {
		Comment: "Added to the score once a recording has been watched.",
	},
	"space.purge.age_weight": {
		Comment: "Added to the score per full week of age.",
	},
	"space.purge.refetchable_weight": {
		Comment: "Added when the broadcast still exists upstream, because deleting a\n" +
			"re-fetchable copy is the cheapest deletion available.",
	},
	"space.purge.protect_for": {
		Comment: "Recordings younger than this never appear in the purge list. Costs\n" +
			"headroom: a long window puts recent broadcasts out of reach exactly\n" +
			"when the library is full.",
	},
	"space.purge.trash_grace": {
		Comment: "How long a purged recording stays recoverable. Costs headroom for\n" +
			"that whole period, because trashed bytes still count against max_size\n" +
			"until they are released.",
	},

	"twitch": {Comment: "The Twitch application this install authenticates as."},
	"twitch.client_id": {
		Comment: "Twitch application id, from dev.twitch.tv/console/apps. Register\n" +
			"your own: create an application, set the client type to public,\n" +
			"and paste its client id here. Empty leaves broadcast titles and\n" +
			"the batched listing a recovery pass reads unavailable.",
	},

	"capture": {Comment: "How broadcasts are captured."},
	"capture.poll_interval": {
		Comment: "How often each channel is checked for a live broadcast.",
	},
	"capture.quality": {
		Comment: "streamlink quality ladder, tried in order until one resolves.",
	},
	"capture.min_duration": {
		Comment: "Recordings shorter than this are marked failed rather than\n" +
			"named into the library, which keeps accidental restarts and\n" +
			"connection tests out of it. The file stays on disk; nothing\n" +
			"here deletes anything.",
	},
	"capture.max_concurrent": {
		Comment: "Maximum simultaneous recordings.",
	},
	"capture.container": {
		Comment: "Container finished recordings are remuxed into. Capture always\n" +
			"writes MPEG-TS first, so a crash mid-broadcast still leaves a\n" +
			"playable file.",
	},

	"naming": {Comment: "How a finished recording is named."},
	"naming.template": {
		Comment: "Path template, relative to the library root. Forward slashes\n" +
			"separate directories on every platform. Every placeholder used\n" +
			"must resolve, so a missing title delays the rename instead of\n" +
			"producing a name with a gap in it.",
		Alternatives: []string{`template = "{channel}/{date} {title}.{ext}"`},
	},
	"naming.timezone": {
		Comment:      "Location dates render in. \"Local\" uses the machine's zone.",
		Alternatives: []string{`timezone = "America/New_York"`},
	},

	"notify": {Comment: "Where alerts go."},
	"notify.desktop": {
		Comment: "Raise a native desktop notification where one can be raised.\n" +
			"macOS and Linux post it from the recorder itself, which runs\n" +
			"inside your session. Windows runs the recorder in session 0,\n" +
			"where there is no desktop, so it needs the notify agent that\n" +
			"'install' registers. With nobody signed in there is nowhere to\n" +
			"show a notification and the event is dropped; the daemon log\n" +
			"and the webhook still have it.",
	},
	"notify.webhook_url": {
		Comment: "POST one JSON object per event here. Empty disables it.\n" +
			"Sent in the background, so a receiver that is slow or gone\n" +
			"never delays a recording. The path is often the only thing\n" +
			"authorizing the post, so it is treated as a secret and never\n" +
			"logged. A credentialed URL is refused, a literal link-local\n" +
			"address is refused, and a redirect is never followed. A name\n" +
			"that resolves to one is not checked, so point this somewhere\n" +
			"you chose.",
		Alternatives: []string{`webhook_url = "https://ntfy.sh/your-topic"`},
	},
	"notify.on_recording_start": {
		Comment: "Notify when a capture begins. Noisy if you watch many channels.",
	},
	"notify.on_failure": {
		Comment: "Notify when a capture or post-processing step fails.",
	},
	"notify.on_library_full": {
		Comment: "Notify when the cap stops a recording. This is the only\n" +
			"event that costs you a broadcast, so leave it on.",
	},

	"channels": {
		Comment: "Channels to watch. Add one [[channels]] block per channel.",
		Alternatives: []string{
			"[[channels]]",
			`platform = "twitch"`,
			`name = "examplechannel"`,
			"enabled = true",
			"backfill = true",
		},
	},
	"channels.platform": {
		Comment: fmt.Sprintf("One of: %s. Twitch has metadata and VOD\n"+
			"recovery; youtube has backfill; url is capture only.",
			strings.Join(SupportedPlatforms, ", ")),
	},
	"channels.name": {
		Comment: "Channel login for twitch and youtube, or the full URL when\n" +
			"platform is \"url\".",
	},
	"channels.enabled": {
		Comment: "Watch this channel. Set false to keep it configured but idle.",
	},
	"channels.quality": {
		Comment: "Quality ladder for this channel. Empty inherits capture.quality.",
	},
	"channels.backfill": {
		Comment: "Let this channel's missed broadcasts be fetched from public\n" +
			"archives. It is the whole permission for that: with it off,\n" +
			"neither the recorder nor 'stream-dvr backfill' downloads a thing\n" +
			"for this channel.",
	},
}

// ///////////////////////////////////////////////
// Defaults
// ///////////////////////////////////////////////

// DefaultConfig returns a configuration that is valid except for
// Library.Root, which has no sensible default and must be set by the
// operator.
func DefaultConfig() Config {
	return Config{
		Version: SchemaVersion,
		Library: Library{
			Root: "",
		},
		Space: Space{
			MaxSize: 2 * Terabyte,
			MinFree: 100 * Gigabyte,
			Recompress: Recompress{
				Enabled:        false,
				After:          30 * Day,
				Codec:          CodecHEVC,
				Quality:        30,
				PreferHardware: true,
				MaxConcurrent:  1,
				KeepOriginal:   false,
			},
			Purge: Purge{
				WatchedWeight:     3,
				AgeWeight:         1,
				RefetchableWeight: 2,
				ProtectFor:        7 * Day,
				TrashGrace:        7 * Day,
			},
		},
		Capture: Capture{
			PollInterval:  Duration(30e9),
			Quality:       []string{"1080p60", "1080p", "720p60", "best"},
			MinDuration:   Duration(120e9),
			MaxConcurrent: 3,
			Container:     "mkv",
		},
		Naming: Naming{
			Template: naming.DefaultTemplate,
			Timezone: "Local",
		},
		Notify: Notify{
			Desktop:          true,
			OnRecordingStart: false,
			OnFailure:        true,
			OnLibraryFull:    true,
		},
		Backfill: Backfill{
			Automatic:     true,
			Settle:        2 * Hour,
			MaxConcurrent: 1,
			MaxAttempts:   5,
		},
		Channels: []Channel{},
	}
}
