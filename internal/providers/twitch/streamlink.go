package twitch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// AuthConfigName is the derived file streamlink reads the header from.
//
// It sits beside config.toml rather than in streamlink's own directory,
// because this file is derived state this project owns and rewrites. An
// operator's own streamlink config is theirs and is never touched.
const AuthConfigName = "streamlink-auth.conf"

// authConfigMode keeps the file to its owner.
//
// It matches config.toml, which already carries a webhook URL that is
// itself a credential. On Windows the perm bits do little and the file
// inherits the data directory's ACL, which comes from the profile root and
// is user-only.
const authConfigMode = 0o600

// ///////////////////////////////////////////////
// The derived config
// ///////////////////////////////////////////////

// WriteAuthConfig writes the one line streamlink needs, returning its path.
//
// # Why a file and not an argument
//
// streamlink accepts the header on a command line or in a config file, and
// offers nothing else: no environment variable, no standard input. A
// command line is readable by other processes. Linux exposes it through
// /proc/<pid>/cmdline, and Windows exposes it to an administrator through
// WMI. A path leaks in both places. A token does not.
//
// The file is passed with --config, which streamlink documents as additive
// rather than replacing, so the operator's own configuration keeps working.
// Never pass --no-config alongside it.
func WriteAuthConfig(dir, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("refusing to write an auth config with no token")
	}
	// One option per line is the file's whole syntax, so a token carrying a
	// line ending writes further options. streamlink's own set includes
	// --player and --ffmpeg-ffmpeg, which name a program to run.
	// ReadAuthConfig hands back only the first matching line, so the hourly
	// credential check would validate a clean token and never see them.
	if strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("refusing to write an auth config with a line ending in the token")
	}
	if err := fsretry.MkdirPrivate(dir, paths.DataDirMode); err != nil {
		return "", fmt.Errorf("creating the data directory: %w", err)
	}

	path := AuthConfigPath(dir)

	// One option per line, dashes omitted, no quoting. streamlink takes a
	// quote literally, so wrapping the token would send the quotes to
	// Twitch as part of the header.
	line := "twitch-api-header=Authorization=OAuth " + token + "\n"

	if err := fsretry.WriteFilePrivate(context.Background(), path, []byte(line), authConfigMode); err != nil {
		// The path is named and the contents are not.
		return "", fmt.Errorf("writing the streamlink auth config at %s: %w", path, err)
	}
	return path, nil
}

// RemoveAuthConfig deletes the derived file.
//
// It runs when a token is found dead, so a capture falls back to public
// quality rather than presenting a credential Twitch has already refused.
// A file that is not there is the desired end state rather than a failure.
func RemoveAuthConfig(dir string) error {
	if err := os.Remove(AuthConfigPath(dir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the streamlink auth config: %w", err)
	}
	return nil
}

// AuthConfigPath reports where the derived file lives, whether or not one
// has been written.
func AuthConfigPath(dir string) string { return filepath.Join(dir, AuthConfigName) }

// ReadAuthConfig recovers the token from the derived file.
//
// The daemon has no other way to reach it. It runs under an S4U logon that
// cannot open the credential store, and the interactive command leaves this
// file for exactly that reason.
//
// The token is returned as a bare string and is never logged, wrapped into
// an error, or placed in an argument by anything that calls this.
func ReadAuthConfig(dir string) (string, error) {
	body, err := os.ReadFile(AuthConfigPath(dir))
	if err != nil {
		// The path is named and nothing else is.
		return "", fmt.Errorf("reading the streamlink auth config: %w", err)
	}

	const prefix = "twitch-api-header=Authorization=OAuth "
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		if token, found := strings.CutPrefix(strings.TrimSpace(line), prefix); found {
			return token, nil
		}
	}
	return "", fmt.Errorf("the streamlink auth config holds no token")
}
