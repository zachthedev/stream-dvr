# Contributing

## The gate

`make check` runs the gates locally. Run it before opening a pull request.
The `Makefile` is the source of truth for what each gate does, and
`.github/workflows/check.yml` runs the same targets one per job. CI adds
what one machine cannot cover: the race detector, and the tests on the
other two platforms.

Never disable a linter, lower a coverage threshold, or delete an assertion
to make a gate pass. A gate that fails is reporting something.

## Platforms

Windows, macOS, and Linux are all supported targets, not one target and
two ports. CI runs the tests and the build on all three. Judge a change
against all three before proposing it, especially anything touching
paths, subprocesses, or autostart registration.

## Generated files

`cmd/generate` writes `config.default.toml` and `config.schema.json` from
`internal/config`, and `.env.template` from `internal/buildenv`. Edit the
Go source, run `make generate`, and commit the output. The `generate` gate
fails when a generated file does not match its source.

## Dependencies

`go.mod` and `go.sum` record which third-party code this project trusts.
Read the diff, then stage them yourself in their own commit. The
pre-commit hook refuses to stage them for you.

Every GitHub Action is pinned to a full commit SHA with the version in a
trailing comment. A tag or a branch is a moving target its owner can
repoint. Dependabot proposes bumps with a three-day cooldown, which is
the only place a minimum release age is enforced for Go.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org).
Put the narrative there: what was wrong, what the change fixes, why an
approach was rejected. Code comments explain why the present code is the
way it is, and reference nothing that is not in the file.

## Claude Code

The committed `.claude/settings.json` pre-approves only commands that
cannot execute anything the working tree supplies. It omits the build and
test verbs, because a checked-out branch supplies the Makefile, the test
code, and the `//go:generate` directives. A reviewer of an untrusted
branch must be asked first.

Approve those for yourself in `.claude/settings.local.json`, which
`.gitignore` covers:

```json
{
  "permissions": {
    "allow": [
      "Bash(go build *)",
      "Bash(go test *)",
      "Bash(go generate *)",
      "Bash(go vet *)",
      "Bash(go mod *)",
      "Bash(golangci-lint *)",
      "Bash(git *)",
      "Bash(mkdir *)"
    ]
  }
}
```

`make` is deliberately absent. A standing approval for it removes the
prompt the paragraph above depends on, and the branch supplies the
Makefile: every value a recipe interpolates is guarded, but a target the
branch adds is not. Run it on a branch you did not write only after
reading its diff of `Makefile`.
