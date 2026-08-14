# stream-dvr

A DVR for live streams: a recording daemon, a TUI client, and a self-describing library on
disk. See `README.md` for the design.

## Load-bearing invariants

Break any of these and the tool loses the property it exists for.

1. **Capture never depends on metadata.** A recording is written under a name derived only
   from platform, channel, and start time. Naming happens later, from validated metadata.
   A metadata failure must never truncate, misname, or lose a recording.
2. **A required name field that is empty blocks the rename.** Never substitute a blank and
   never emit a name with a missing segment. The file waits in `incoming/` instead.
3. **The daemon never deletes a recording.** Retention ranks; the operator deletes. The
   only automatic response to a full library is to stop recording and notify.
4. **External tool paths are resolved per use.** Never cache a resolved path across
   operations. Versioned install directories get renamed on upgrade.
5. **Library ownership is a build tag, not config.** `internal/library.BuildOwner` is fixed
   at compile time. Nothing may override it at runtime.

## Build tags

`make build` produces a production binary. `make build TAGS=dev` produces one that can open
only development sandboxes. Dev-only code lives in `*_dev.go` guarded by `//go:build dev`,
paired with a `!dev` file declaring the same symbol.

## Git Hooks

Git hooks are managed by [lefthook](https://github.com/evilmartians/lefthook).
Configuration is in `lefthook.yml`. Run `go tool lefthook install` once after
cloning.

## Conventions

- Go 1.26, `gofumpt`, `slog` for logging, error wrapping with `%w`
- Never use em dashes; use colons, semicolons, commas, or parentheses
- Do not duplicate canonical data; reference the source file instead
- Test files are 1:1 with source files; no scatter
- Every quality gate must pass before merging: `make check`
