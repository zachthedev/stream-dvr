# stream-dvr

A set-and-forget DVR for live streams. It watches channels, records broadcasts to a
self-describing library, backfills what it missed from public archives, and shows coverage
on a calendar so gaps are obvious.

Capture runs as a background daemon. The TUI is a client, so closing it never stops a
recording. The one exception is a recorder the TUI started itself with `d`. That one runs
inside the calendar's own process and stops when the window closes.

On Windows and Linux the daemon keeps recording with nobody signed in, through a scheduled
task with a boot trigger and through a lingering systemd user unit. **On macOS it does not.**
The recorder is a launchd agent in your own session. A system daemon runs as root and cannot
read the streamlink credentials in your home directory, so it would record only what is
public. The cost of that trade is that a Mac sitting at the login window records nothing.

## Commands

- `stream-dvr` with no arguments opens the calendar, which is the interface to everything else
- `stream-dvr version` prints the build version and which library lineage the binary owns
- `stream-dvr doctor` resolves every external tool, reports versions, and checks locations
- `stream-dvr config init|validate|path` writes and checks the configuration file
- `stream-dvr library init|adopt` creates or claims a library, writes its ownership marker, and
  points the config at it
- `stream-dvr library import` records library files no recording names yet, reading each one's
  sidecar where there is one and its filename where there is not, and `--dry-run` reports what
  it would adopt without writing
- `stream-dvr auth twitch` stores the recording token, and `auth twitch metadata` authorizes
  the listing API, which first needs a Twitch application id in `twitch.client_id`
- `stream-dvr serve` runs the recording daemon until interrupted, recovering what it missed
  while it was not running
- `stream-dvr backfill --since 3d` fetches past broadcasts the recorder missed over that
  range, and `--dry-run` reports what it would fetch without fetching
- `stream-dvr install|uninstall|status` registers the daemon to start with the machine

`--config` selects the configuration file and works at any depth, so
`stream-dvr --config x.toml serve` and `stream-dvr serve --config x.toml` mean the same thing.

The calendar does more than show coverage. `e` edits every setting and the watched channel
list, through the same validation the daemon reads with. `x` opens the assisted purge: a
ranking of what is cheapest to lose, with the reason each recording scored where it did, and
one confirmation for the whole selection. `d` runs the recorder in this window, which is the
quickest way to try a configuration without registering a service. One library takes one
recorder, and a second start against a library another recorder holds is refused.

The recorder recovers what it missed on its own. After a restart it measures how long it was
down and fetches the broadcasts that ended while it was, whether it crashed or stopped cleanly.
A freeze does the same, so a laptop that slept through a broadcast still gets it. An outage that
ends covers the stretch the platform was unreachable. A routine round every six hours picks up a
stream shorter than one poll, or a hole inside a capture.

A round tells you what it did once, when it ends, rather than once per broadcast: a fortnight of
downtime recovers by the dozen, and that many notifications is a burst nobody reads. The one
exception is a broadcast it gave up on, which needs you and so is raised on its own.

A round waits for three things. It will not start while a capture is running, because both write
to one volume and pull one link and only the recording is irreplaceable; a round already in
flight stops when a broadcast begins. It will not start while no platform has answered a probe,
since fetching against a service that is down achieves nothing. And rounds are paced apart, so a
link that keeps dropping cannot drive one per drop. A hold that has not cleared in twelve hours
is reported rather than left silent.

A fetch that fails waits twice as long before the next try, up to a day. Nothing counts tries
and gives up: only the platform answering that a video is removed, private, or behind a
subscription retires a broadcast, because that answer will not change and every other one might.
An afternoon of bad network therefore costs time and nothing else, and a broadcast nobody can
download stops being asked for when it ages out of the two-week window.

Filling a hole inside a broadcast already captured is the opposite trade, and does count tries.
The recording is on disk either way, so giving up on a hole costs a stretch rather than a whole
broadcast. `backfill.max_attempts` is that count.

Automatic recovery never reaches back further than two weeks, which is about as long as an
ordinary channel's copies survive upstream, and never further back than the day this library
first recorded anything. A recorder cannot have missed what aired before it existed.

`backfill.automatic = false` turns all of that off, leaving a recorder that keeps recording and
stops filling its own gaps. It ships on.

`stream-dvr backfill --since 3d` is the one-off, for reaching further back than that or for not
waiting. There is no default range: a pass downloads hours of video from somebody else's
service, so the range is yours to pick. Only channels with `backfill = true` are considered,
either way. The command refuses while a recorder holds the library, because a second writer
would race its sweep. Requesting a single missed day from the calendar with `R` is not wired,
and the key says so.

## Design

### The recorder never names the file

Capture writes to `incoming/<platform>-<channel>-<start-unix>.ts`, a name that needs no
metadata. A separate poller tracks title, category, and viewer count across the whole
broadcast into SQLite and a JSON sidecar. At finalize, an organizer renders the filename
from validated metadata.

A required field that is empty blocks the rename rather than producing a partial name. The
file stays in `incoming/` and the TUI flags it. A metadata failure can therefore delay a
name and never damage a recording.

### Library layout

```text
<root>/
├── .dvr/            database, ownership marker, trash, logs
├── incoming/        in-progress and not-yet-named captures
└── <Channel>/<Year>/<Channel> - <date> - <title>.mkv + .json
```

The sidecar JSON beside each recording carries everything the database holds, so the
database is always rebuildable from a bare directory of files.

### Timestamps are integers, and the views make them readable

SQLite has no time type, so timestamps are Unix nanoseconds in `INTEGER` columns. Text cannot
do this safely: any layout wide enough to hold a fraction sorts a value carrying one against a
value without, and `.` sorts below every digit. A recording starting a fraction of a second
after a month boundary lands on the wrong side of it and vanishes from both the month query and
the coverage calendar.

Every table has a companion view rendering those columns as RFC 3339 under a `_utc` suffix, so
reading the database by hand does not mean reading epoch integers:

```sh
sqlite3 .dvr/library.db "SELECT path, started_at_utc FROM recordings_readable LIMIT 5"
```

### Coverage says what it can prove

A day is only as covered as its recordings. A capture that failed leaves bytes on disk and
proves nothing, so it reads as missed rather than covered. A capture that finished without
reaching the library, held by a missing title or a locked file, is real but unfinished. It
gets its own state rather than the colour of a verified day. A recording stuck for a week is
exactly what a reassuring calendar would hide.

### Space is a ladder, and only you cross the line that loses recordings

The library has a hard size cap and a free-space floor. What happens as the library approaches
them is a ladder, ordered by what each rung costs. The `[space]` section of the config lists
them in the same order, because the reading order is the escalation order.

Everything down to recompress changes how much space a recording takes. Purging and refusing
change how many recordings you have. Only you cross that line: nothing on it runs on its own.

**Remux** costs nothing. Capture writes MPEG-TS first, so a crash mid-broadcast still leaves a
playable file. The organizer then stream-copies that into the configured container. Nothing is
re-encoded and nothing is decided, so there is no setting for it.

**Release trash** costs the undo window on files already condemned. A purge moves the
recording to `.dvr/trash` and frees nothing: the file is still on the volume, so it still
counts against the budget, and it is the release at the end of `space.purge.trash_grace` that
returns the bytes. Releasing early gives up the rest of the window. The recorder releases
expired purges only while the library is low or critical, oldest first, and stops the moment
the level clears. A library with room to spare keeps its whole undo window.

**Recompress** costs picture quality, permanently, and costs no broadcasts. Older recordings
are decoded and encoded again to a denser codec. It is the last rung that frees space without
losing a recording. It is off by default and opted into per machine, because a machine with no
hardware encoder falls back to software at hours per broadcast. Run `stream-dvr doctor` to see
the encoder this machine would actually use. Once enabled, the recorder re-encodes recordings
past `space.recompress.after` on a slow timer, oldest first and one at a time. The original is
set aside rather than deleted until the re-encode is verified and recorded, so a failed pass
costs time and never a broadcast.

**Purge** costs whole broadcasts and keeps the quality of everything that remains. The scoring
ranks candidates by age, by whether the recording is marked watched, and by whether the
broadcast still exists upstream. A re-fetchable copy is the cheapest deletion available, and a
sole surviving copy is the most expensive. Nothing is deleted automatically. The scoring ranks
and never acts, so a deletion can only start with a keypress and end with one confirmation.
Keeping those apart is checkable rather than promised. `x` in the calendar opens the ranking,
which names the reason each recording scored where it did.

**Refuse** costs a broadcast that has not happened yet. It is not a rung you choose. It is
where the ladder falls to when nothing above it runs. Reaching the cap or the floor stops the
next recording and notifies, and it is the only outcome here that costs something unrecorded.

### Another program holding a file delays it, never fails it

On Windows a backup agent, indexer, or scanner that opens a recording without
`FILE_SHARE_DELETE` blocks every rename and delete of it. Reads still work, so the failure
appears only at the moment the organizer moves the finished file into the library.

Go reports this as a sharing violation, which does not satisfy `errors.Is(err,
fs.ErrPermission)`, so a permission check sees nothing and the move looks unrecoverable.
`internal/fsretry` matches the condition explicitly and waits it out with a bounded backoff.

Backing up a multi-gigabyte recording takes far longer than any call can block, so a hold that
outlives the window parks the recording in `awaiting_file` instead. That is the same holding
state a missing title uses, and the daemon's sweep retries it every 15 minutes until the file
comes free. The recording stays intact and playable under its capture name throughout.

### External tools are resolved at the point of use

`internal/deps` locates streamlink, ffmpeg, ffprobe, and yt-dlp on every use rather than
caching a path at startup. Package managers install into versioned directories and rename
them on upgrade, so a path captured hours earlier can point at a directory that is gone.
Resolution tries an environment override, then `PATH`, then a case-insensitive search of
per-package directories.

## Development sandboxing

A development build cannot touch a real library. Ownership is fixed at compile time by a
build tag, not by config, because config can be edited or mistyped.

```sh
make build              # production binary, owns "prod" libraries
make build TAGS=dev     # development binary, owns "dev" libraries only
```

Every library carries `.dvr/library.json` naming its owner. Opening a library whose owner
does not match the binary's own lineage fails:

```text
library D:\recordings is owned by the prod build, this is the dev build
```

`library adopt` is the only way a populated directory becomes a library, so claiming one is
always deliberate.

## Quick start

```sh
make build
./dist/stream-dvr doctor
./dist/stream-dvr library init /path/to/library
./dist/stream-dvr doctor --library /path/to/library
```

`doctor` exits non-zero when a required tool is missing. Pass `--verbose` to see each
resolved path.

### Your own Twitch application

Recording needs none of this. The metadata API is what carries broadcast titles and lets a
recovery pass list a channel in one request instead of one per broadcast, and reaching it
needs an application id:

1. Open <https://dev.twitch.tv/console/apps> and register an application.
2. Set the client type to **public**, which is what a device code flow needs. There is no
   client secret to store.
3. Put its client id in `twitch.client_id`, then run `stream-dvr auth twitch metadata`.

No id ships in the binary, and that is deliberate. One baked in would make every download
act as the same registration, which names whoever produced the build as the developer
answerable for what any of them does. A client id is public rather than secret, so keeping
it in your config costs nothing to protect.

## Prerequisites

- Go 1.26+
- streamlink, ffmpeg, ffprobe, and yt-dlp on `PATH` or in a standard package location

Build and lint tooling: `golangci-lint` v2, `gofumpt`, `goimports`, and the `lefthook`,
`testpair`, and `deadcode` tools pinned in `go.mod`. Run `make check` for the full gate set.

## Platforms

Windows, macOS, and Linux are supported targets rather than one target and two ports.

Autostart is per user on all three. A machine-wide service runs as an account that cannot read the
streamlink credentials in your home directory, so it would record only what is public and silently
miss subscriber and ad-free streams.

|                   | Windows                | macOS                    | Linux                    |
| ----------------- | ---------------------- | ------------------------ | ------------------------ |
| Autostart         | Scheduled Task         | launchd user agent       | systemd user unit        |
| Registered in     | the task scheduler     | `~/Library/LaunchAgents` | `~/.config/systemd/user` |
| Hardware encoding | NVENC, Quick Sync, AMF | VideoToolbox             | NVENC, Quick Sync, VAAPI |
| Config file mode  | not applied, see below | `0600`                   | `0600`                   |

`status` names the mechanism the host uses. Encoder selection probes the host rather than trusting
the list, because ffmpeg advertises encoders the installed driver cannot actually run.

The catalogue has no AV1 entry for VideoToolbox, because no Mac encodes AV1 in hardware. Apple
silicon decodes AV1 and does not encode it. Choosing AV1 on a Mac therefore encodes in software,
which for a multi-hour capture runs longer than the broadcast itself.

The Windows task runs as you before anyone signs in, and the systemd unit enables linger so it
survives logout. Both are ways of saying the same thing: the recorder is up whether or not there is
a session.

Registering the scheduled task on Windows needs an elevated shell once, because importing a task
definition is an administrative operation whatever principal the task names. Recording itself needs
no elevation, and neither a systemd user unit nor a launchd agent needs any.

### The config file's mode is enforced on Linux and macOS only

The config file is written `0600`. On Linux and macOS that means the owner alone can read it.

Windows ignores it. Go translates a Unix mode to Windows as a single bit, setting
`FILE_ATTRIBUTE_READONLY` when the write bit is clear, and `0600` has the write bit. The file
therefore inherits the ACL of the profile directory, which grants full control to SYSTEM, to the
local administrators, and to you. Anything running as your account can read it. Treat a token stored
there as readable by every process you run.

### A library does not move between a case-sensitive and a case-insensitive filesystem

Two broadcasts whose rendered names differ only in case are one name on NTFS and on the default
APFS, and two names on ext4. Nothing is lost in either direction. The organizer sees the existing
file and moves both the recording and its sidecar to a " (2)" suffix, and `fsretry.RenameNew` opens
its destination `O_EXCL` so a rename refuses an occupied name rather than overwriting it.

The residual is portability. A library built on ext4 holds
`ExampleChannel - 2026-01-01 - Title.mkv` and `ExampleChannel - 2026-01-01 - title.mkv` as two
recordings with two rows, because `recordings.path` is unique under a binary collation. Copy that
library to a Mac or to Windows and the two files collide, while the database still expects both.
Move a library across filesystems with a copy that reports collisions instead of resolving them,
and settle any it finds before opening the library on the new machine.

### Free space reads low on APFS

The free-space floor counts the blocks available to an unprivileged user, which is the number this
tool can actually spend. APFS excludes purgeable space from that count: snapshots and caches macOS
reclaims on demand. A Mac that Finder says has 200 GB free reports a smaller figure here, so the
floor trips earlier than Finder suggests it will. Set the floor against the number this tool
reports rather than the one Finder shows.
