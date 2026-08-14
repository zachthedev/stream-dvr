// Command version is a build-time helper. It prints the SemVer 2.0.0
// string for the current git state to stdout, intended for use in a
// Makefile ldflags injection:
//
//	VERSION ?= $(shell go run ./internal/tools/version)
//	LDFLAGS ?= -s -w -X <module>/internal/version.semver=$(VERSION)
//
// Not shipped in release artifacts. End-user version display happens at
// runtime via internal/version.Info() on the shipped binary.
package main

import (
	"fmt"

	"zach.tools/go/stream-dvr/internal/version"
)

func main() {
	fmt.Print(version.FromGit())
}
