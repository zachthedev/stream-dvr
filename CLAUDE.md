# stream-dvr

A set-and-forget DVR for live streams. It watches channels, records broadcasts
to a self-describing library, fills in what it missed from public archives, and
shows coverage on a calendar so gaps are visible rather than assumed.

Go 1.26.6, module `zach.tools/go/stream-dvr`, about 30 internal packages.

## The shape of it

Three programs' worth of behaviour in one binary:

- **the daemon** (`serve`) polls each channel, captures with streamlink, and
  hands finished files to the organizer
- **the recovery pass** lists past broadcasts, fetches what the recorder
  missed with yt-dlp, and patches holes inside captures it did make
- **the TUI** (the bare command) is a _client_. Closing it never stops a
  recording

Capture writes MPEG-TS into `incoming/` under a name that needs no metadata.
Only once it is remuxed, named, and moved does a recording count as being in
the library. That ordering is deliberate: a row claiming completion before the
move would leave files outside the sweep, counted as covered.

### Where things live

| Package     | Holds                                                           |
| ----------- | --------------------------------------------------------------- |
| `daemon`    | the watch loop, the capture cycle, the notification fan-out     |
| `record`    | the streamlink driver, and nothing else                         |
| `fetch`     | the yt-dlp driver, and nothing else                             |
| `backfill`  | _why_ a past broadcast is worth fetching, and gap patching      |
| `store`     | SQLite through modernc, schema created whole at version 1       |
| `library`   | the root, its ownership marker, and its state directory         |
| `organize`  | remux, name, move; the only thing that finalizes                |
| `naming`    | the filename template and its fallbacks                         |
| `calendar`  | which days a channel is covered                                 |
| `providers` | one subpackage per site; `providers.go` holds only the contract |
| `config`    | the TOML config, its validation, and its file permissions       |
| `secret`    | credentials, in one 0600 file the daemon can also write         |
| `service`   | scheduled task and systemd registration                         |
| `notify`    | desktop, webhook, and tray sinks                                |
| `tui`       | the calendar client                                             |
| `buildenv`  | the build-time settings `.env.template` is generated from       |
| `generate`  | the registry behind `make generate`                             |

Everything else (`escape`, `paths`, `deps`, `space`, `procgroup`, `fsretry`,
`post`, `retention`, `logger`, `migrate`, `remote`, `version`) is support.

## Things worth knowing before changing anything

- **Two Twitch credentials exist and are not interchangeable.** The browser
  cookie records. The device-flow token from `auth twitch metadata` reads the
  Helix API and is _refused_ by Twitch's playback endpoint. A device token
  yields zero streams where no token yields the full quality ladder, so handing
  one to streamlink takes the recorder offline.
- **A refresh token is spent when used.** `twitch.Session.renew` stores the new
  pair _before_ returning the access token, and holds a mutex while doing it. A
  crash between the exchange and the write loses the session permanently.
- **streamlink echoes its config file at debug level.** `captureLogLevel` is
  `info` on purpose, because the config file holds a token.
- **Never put a credential in an error.** Errors get wrapped, logged, and
  pasted into bug reports. The packages that touch credentials assert this
  about themselves in their doc comments, so keep it true.
- **Remote text is untrusted.** Titles reach filenames and terminals. `escape`
  is the mitigation every `//nolint:gosec // G705` points at.
- **Coverage exclusions are argued, not waived.** Each entry in
  `.testcoverage.yml` says what it costs and what is tested instead.
- **`//nolint` names the rule and gives a reason.** No bare ones.

## Developer experience

### The gate

```sh
make check      # tidy generate lint testpair deadcode vulncheck
                # test coverage build dist
make test TAGS=dev
```

Run the whole target, not the pieces. Running them individually is how `tidy`,
`vulncheck`, and `dist` get skipped. Each of the three failed alone while the
other two passed.

Recipes are POSIX shell. **Run `make` from Git Bash**, not PowerShell, or the
targets that probe for a tool fail on `command -v`.

Specific targets worth knowing:

- `make deadcode`: unreachable exported symbols, against `.allow.deadcode`.
  Every entry needs a `# [category]` comment or the check fails
- `make testpair`: every `foo.go` needs a `foo_test.go`, and test names must
  match a real symbol
- `make generate`: regenerates `config.default.toml`, `config.schema.json`,
  and `.env.template`, then fails if any differ from what is committed

### What the local gate cannot see

Two blind spots. CI is the only place either surfaces:

- **The race detector never runs here.** It needs cgo, and this builds
  `CGO_ENABLED=0` because the SQLite driver is modernc's pure-Go one.
- **Only one platform runs here.** `lint` compiles for three GOOS and `dist`
  links five targets, but no test executes on any platform but this one.

Four shapes of bug can cross that gap, and CI caught each one:

- a test that silently needs yt-dlp installed
- a Linux-only assertion untrue of any machine with a session bus
- a fixture absolute on Windows and relative elsewhere
- a `\n`-delimited assertion that fails on a CRLF checkout

**Push a change to a subprocess driver, to anything with a
`_linux`/`_darwin`/`_windows` suffix, or to a path fixture, and let CI judge
it.**

### Build settings

`.env` at the repo root, gitignored, read by the Makefile via `-include`. Its
template is generated from `internal/buildenv`, so the two cannot describe
different variables. `make generate` fails if the template is stale.

Nothing here is required. The Twitch application id is not a build setting:
it is config, under `twitch.client_id`, so every install registers its own
rather than sharing whichever one a release was built with.

### Releases

Manual, in two halves. `prepare.yml` keeps a release PR open with the next
version and changelog, and merging it writes the tag. That tag carries the
workflow's own token, which GitHub refuses to let start another workflow, so
run `deliver.yml` by hand against it. That half builds five targets through
goreleaser and publishes them with checksums.

### Repository settings no file records

GitHub settings survive no clone, and a recreated repo comes back with
defaults. Reapply:

- **Actions may create and approve pull requests: on.** `prepare.yml` opens its
  PR with `GITHUB_TOKEN`, which GitHub refuses without this. A repository that
  releases through a GitHub App does not need it, so copying settings from one
  that does switches release-please off here.
- **Private vulnerability reporting: on.** It is off by default on a new
  repository, and `SECURITY.md` sends reporters to the button it adds. Without
  it the policy names a channel that is not there.
- Secret scanning and push protection on; issues, projects, wiki on; downloads
  off; merge, squash, and rebase all allowed; auto-merge off; branch deletion
  on merge off; default branch `main`; no rulesets; immutable releases off.

## Conventions

Commits are Conventional Commits. Comments explain _why_, never what changed.
Change narrative belongs in the commit message, where `git blame` attaches it
to the diff. A reader who never saw an earlier version must not be able to tell
one existed.

Tests encode intent, not observed output. Derive the assertion from the
requirement, then run it. A test written by pasting what the code returned
passes on day one and passes just as happily once the behaviour breaks.
