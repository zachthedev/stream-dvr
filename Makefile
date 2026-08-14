# Build configuration. Override on the command line (e.g. GOOS=linux make build).
GOOS ?=
GOARCH ?=
CGO_ENABLED ?= 0
CC ?=

# Build target(s). Override on the command line for selective builds.
BUILD_TARGET ?= ./cmd/stream-dvr

# Name of the produced binary, used to name the per-platform artifacts.
BINARY ?= $(notdir $(BUILD_TARGET))

# GOOS values the lint gate covers. golangci-lint analyses one GOOS per run,
# so every file behind a build tag for another platform is seen by nothing.
LINT_GOOS ?= linux darwin windows

# Platforms `dist` produces an artifact for, as GOOS/GOARCH pairs.
PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

# Build tags. Empty by default, so `make build` can never produce a binary
# that opens a development sandbox. Use TAGS=dev for local work.
TAGS ?=
ifeq ($(strip $(TAGS)),)
GO_TAG_FLAGS :=
else
GO_TAG_FLAGS := -tags $(TAGS)
endif

# Local build settings, if there are any. Included before every assignment
# below so a value set there wins the ?= that follows it.
#
# The leading dash treats a missing file as ordinary rather than an error.
# .env is not committed, so most builds have none. Its committed template is
# generated from internal/buildenv, so the two cannot describe different
# variables.
-include .env

# Version string injected via -ldflags -X at build time.
VERSION ?= $(shell go run ./internal/tools/version 2>/dev/null)

# Import path of the internal/version package for -ldflags -X injection.
VERSION_PKG ?= zach.tools/go/stream-dvr/internal/version

# GitHub owner/repo read from the git remote. Empty until the repo is
# published. Override on the command line (REPO_OWNER=foo REPO_NAME=bar)
# for a local build before that.
#
# Both captures are restricted to the characters GitHub allows in a name.
# The value lands unquoted in -ldflags, where whitespace starts a new
# argument. An unrestricted match therefore lets a crafted remote URL append
# a second -X, which wins by last write and stamps a version the binary was
# never built from. A name outside the charset yields empty, which reads the
# same as an unpublished repo, and no name is silently trimmed to fit.
REPO_OWNER ?= $(shell git remote get-url origin 2>/dev/null | sed -n 's|.*github\.com[:/]\([A-Za-z0-9-]\{1,39\}\)/.*|\1|p')
REPO_NAME ?= $(shell git remote get-url origin 2>/dev/null | sed -n 's|.*github\.com[:/][^/]*/\([A-Za-z0-9_-]\{1,100\}\).*|\1|p')

# The captures above restrict only what git supplies. A value from the
# environment, the command line, or the settings file reaches a recipe
# without passing one, and `?=` lets any of them win. Whitespace in -ldflags
# starts a new argument, so a second -X lands and wins by last write. A
# single quote closes the shell word the flag list sits in, so what follows
# it is a command the recipe runs. `override` beats a command-line
# assignment, which a plain `:=` does not.
#
# One definition, applied to every value that reaches a shell word. The
# settings file is read as makefile syntax before any of the assignments
# below, so anything a recipe interpolates has to come through here: the
# file is gitignored and no branch carries one, but a fork can force-add it
# and CONTRIBUTING tells a reviewer to run `make check` on a branch they
# did not write.
word1 = $(firstword $(subst ',,$(1)))

override VERSION := $(call word1,$(VERSION))
override REPO_OWNER := $(call word1,$(REPO_OWNER))
override REPO_NAME := $(call word1,$(REPO_NAME))
override GOOS := $(call word1,$(GOOS))
override GOARCH := $(call word1,$(GOARCH))
override CGO_ENABLED := $(call word1,$(CGO_ENABLED))
override BINARY := $(call word1,$(BINARY))

# Import path of the internal/remote package for -ldflags -X injection.
REMOTE_PKG ?= zach.tools/go/stream-dvr/internal/remote

# The Twitch application id is not built in. It is config, under
# twitch.client_id, because every install registers its own at
# dev.twitch.tv/console/apps. One injected here would make every download of
# a release act as the same registration, which names whoever produced the
# build as the developer answerable for what any of them does.

# Guarded for the same reason, and more urgently: CC lands in a
# DOUBLE-quoted shell word below, where a $( ) or a backtick is evaluated
# without needing to close the quote at all.
override CC := $(call word1,$(CC))

# The target is a path handed to the compiler. Guarded so a value carrying
# a shell metacharacter cannot become a second command, and quoted at every
# point of use.
override BUILD_TARGET := $(call word1,$(BUILD_TARGET))

# Linker flags. Injects VERSION and owner/repo into the shipped binary.
#
# Single-quoted at the point of use below. Every value here comes from a git
# tag, a git remote, or the environment, all of which a clone chooses, and a
# double-quoted shell word evaluates a backtick or a $( ) inside it.
#
# Assembled here and nowhere else. Guarding the four values that go into it
# while leaving the string itself overridable guards nothing: one single
# quote in it closes the shell word the recipe wraps it in, and the rest is
# a command. `override` is what makes that true of a command-line
# assignment as well, which beats a plain `:=`.
#
# The three inputs above are the supported way to change what is injected.
override LDFLAGS := -s -w \
	-X $(VERSION_PKG).semver=$(VERSION) \
	-X $(REMOTE_PKG).ldOwner=$(REPO_OWNER) \
	-X $(REMOTE_PKG).ldRepo=$(REPO_NAME)

# Files cmd/generate writes, asked of the generator so the drift check cannot
# scope itself to a stale list.
#
# The check names them rather than diffing the whole tree. An unscoped diff
# reports every uncommitted edit as stale generation.
GENERATED ?= $(shell go run ./cmd/generate list outputs)

# ///// Canonical form /////
.PHONY: tidy generate fmt

tidy: # ensure go.mod/go.sum are canonical
	go mod tidy
	@git diff --exit-code go.mod go.sum || { echo ""; echo "FAIL: go.mod/go.sum not tidy. Run 'go mod tidy' and commit."; exit 1; }

# Invoked directly rather than through go generate ./..., which would run
# every //go:generate directive the checked-out tree declares. CI builds a
# fork's branch, so that is an arbitrary-command primitive bought for
# nothing: this module has one generator and names it here.
generate: # regenerate the checked-in generated files
	go run ./cmd/generate run
	@[ -n "$(GENERATED)" ] || { echo "FAIL: 'go run ./cmd/generate list outputs' named no files, so nothing would be checked."; exit 1; }
	@git diff --exit-code $(GENERATED) || { echo ""; echo "FAIL: generated files are stale. Run 'make generate' and commit."; exit 1; }

fmt: # apply gofumpt and goimports
	@ok=1; \
	 command -v gofumpt   >/dev/null 2>&1 || { echo "gofumpt not found: go install mvdan.cc/gofumpt@latest"; ok=0; }; \
	 command -v goimports >/dev/null 2>&1 || { echo "goimports not found: go install golang.org/x/tools/cmd/goimports@latest"; ok=0; }; \
	 [ $$ok -eq 1 ] || exit 1
	gofumpt -w .
	goimports -w .

# ///// Static analysis /////
.PHONY: lint vet testpair deadcode

# The config is verified before it is used. `run` accepts settings the schema
# rejects, and CI verifies before it lints. Without this line the two
# disagree, and the disagreement surfaces only on a push.
lint: # run golangci-lint once per supported GOOS
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found: https://golangci-lint.run/usage/install/"; exit 1; }
	@golangci-lint config verify
	@for goos in $(LINT_GOOS); do \
		echo "golangci-lint: GOOS=$$goos"; \
		GOOS=$$goos golangci-lint run || exit 1; \
	done

vet: # run go vet
	go vet ./...

testpair: # verify 1:1 source/test file pairing
	go tool testpair $(ARGS) ./cmd/... ./internal/...

# The wrapper and the analyzer it runs are both named deadcode, so the
# module path is what picks one. The analyzer is a tool dependency, which
# builds it with the toolchain in use and keeps it level with the code.
deadcode: # report unreachable exported symbols
	go tool zach.tools/go/devtools/cmd/deadcode $(ARGS)

# ///// Security /////
.PHONY: vulncheck

vulncheck: # scan dependencies for known CVEs
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not found: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck ./...

# ///// Behavior /////
.PHONY: test coverage

test: # run the full test suite
	go test -count=1 $(GO_TAG_FLAGS) ./...

coverage: # run tests and enforce per-package / total thresholds
	@command -v go-test-coverage >/dev/null 2>&1 || { echo "go-test-coverage not found: go install github.com/vladopajic/go-test-coverage/v2@latest"; exit 1; }
	go test -coverprofile=coverage.out -covermode=atomic ./cmd/... ./internal/...
	go-test-coverage --config .testcoverage.yml

# ///// Artifact /////
.PHONY: build dist clean

build: # build one binary into dist/, for the host unless GOOS/GOARCH are set
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) CC="$(CC)" \
		go build -trimpath $(if $(LDFLAGS),-ldflags '$(LDFLAGS)') $(GO_TAG_FLAGS) -o dist/ "$(BUILD_TARGET)"

# Named per platform rather than written to one path, because two of the
# targets share a GOOS and would otherwise overwrite each other.
dist: # cross-compile every released platform into dist/
	@for platform in $(PLATFORMS); do \
		goos=$${platform%/*}; \
		goarch=$${platform#*/}; \
		suffix=''; \
		[ "$$goos" = windows ] && suffix=.exe; \
		out="dist/$(BINARY)_$${goos}_$${goarch}$$suffix"; \
		echo "building $$out"; \
		CGO_ENABLED=$(CGO_ENABLED) GOOS=$$goos GOARCH=$$goarch \
			go build -trimpath $(if $(LDFLAGS),-ldflags '$(LDFLAGS)') $(GO_TAG_FLAGS) \
			-o "$$out" "$(BUILD_TARGET)" || exit 1; \
	done

clean: # remove build outputs and coverage profile
	rm -rf dist/ coverage.out

# ///// Aggregates /////
.PHONY: check

check: tidy generate lint testpair deadcode vulncheck test coverage build dist # local CI mirror
