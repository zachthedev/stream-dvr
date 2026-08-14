package twitch

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// WriteAuthConfig
// ///////////////////////////////////////////////

func TestWriteAuthConfig_WritesTheLineStreamlinkReads(t *testing.T) {
	// One option per line, dashes omitted, no quoting. streamlink takes a
	// quote literally, so wrapping the token would send the quotes to
	// Twitch as part of the header value.
	dir := t.TempDir()

	path, err := WriteAuthConfig(dir, sentinelToken)
	if err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written config: %v", err)
	}

	want := "twitch-api-header=Authorization=OAuth " + sentinelToken + "\n"
	if string(body) != want {
		t.Errorf("wrote %q, want %q", string(body), want)
	}
}

func TestWriteAuthConfig_KeepsTheFileToItsOwner(t *testing.T) {
	// The token sits here for as long as the daemon runs, so the mode is
	// the only thing between it and another account on the machine.
	if runtime.GOOS == "windows" {
		t.Skip("Go's perm bits do not carry on Windows; the ACL is inherited from the data directory")
	}
	dir := t.TempDir()

	path, err := WriteAuthConfig(dir, sentinelToken)
	if err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != authConfigMode {
		t.Errorf("mode = %o, want %o", got, authConfigMode)
	}
}

func TestWriteAuthConfig_LandsBesideTheOtherConfiguration(t *testing.T) {
	// It is derived state this project owns and rewrites. The operator's
	// own streamlink config is theirs and is never touched.
	dir := t.TempDir()

	path, err := WriteAuthConfig(dir, sentinelToken)
	if err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}
	if got, want := filepath.Dir(path), dir; got != want {
		t.Errorf("wrote into %q, want %q", got, want)
	}
	if got, want := filepath.Base(path), AuthConfigName; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

func TestWriteAuthConfig_CreatesTheDirectoryItNeeds(t *testing.T) {
	// A first run authenticates before anything else has made the data
	// directory.
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")

	if _, err := WriteAuthConfig(dir, sentinelToken); err != nil {
		t.Errorf("WriteAuthConfig() err = %v, want nil for an absent directory", err)
	}
}

func TestWriteAuthConfig_RefusesToWriteAnEmptyCredential(t *testing.T) {
	// A file saying OAuth and nothing else would make streamlink send a
	// malformed header on every capture, which reads as an auth failure
	// rather than as the missing token it is.
	tests := []string{"", "   ", "\t\n"}

	for _, token := range tests {
		if _, err := WriteAuthConfig(t.TempDir(), token); err == nil {
			t.Errorf("WriteAuthConfig(%q) err = nil, want a refusal", token)
		}
	}
}

func TestWriteAuthConfig_ReplacesWhatWasThere(t *testing.T) {
	// Re-authenticating rewrites the file. Appending would leave streamlink
	// reading the first line and the operator wondering why a fresh token
	// changed nothing.
	dir := t.TempDir()

	if _, err := WriteAuthConfig(dir, "OLDTOKENOLDTOKENOLDTOKEN000000"); err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}
	path, err := WriteAuthConfig(dir, sentinelToken)
	if err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written config: %v", err)
	}
	if strings.Contains(string(body), "OLDTOKEN") {
		t.Errorf("the old token survived the rewrite: %q", string(body))
	}
	if strings.Count(string(body), "twitch-api-header") != 1 {
		t.Errorf("wrote %d header lines, want 1", strings.Count(string(body), "twitch-api-header"))
	}
}

func TestWriteAuthConfig_FailureDoesNotCarryTheToken(t *testing.T) {
	// A path that cannot be written is the ordinary way this fails, and the
	// error goes straight to the operator's terminal.
	blocker := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("preparing the blocked path: %v", err)
	}

	_, err := WriteAuthConfig(filepath.Join(blocker, "under"), sentinelToken)
	if err == nil {
		t.Skip("the filesystem allowed a path under a regular file")
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("the error carries the token: %q", err.Error())
	}
}

// ///////////////////////////////////////////////
// RemoveAuthConfig
// ///////////////////////////////////////////////

func TestRemoveAuthConfig_TakesTheFileAway(t *testing.T) {
	// It runs when a token is found dead, so a capture falls back to public
	// quality rather than presenting a credential Twitch already refused.
	dir := t.TempDir()

	path, err := WriteAuthConfig(dir, sentinelToken)
	if err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}
	if err := RemoveAuthConfig(dir); err != nil {
		t.Fatalf("RemoveAuthConfig() err = %v, want nil", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the auth config survived removal")
	}
}

func TestRemoveAuthConfig_IsSafeToRepeat(t *testing.T) {
	// A file that is not there is the state the caller asked for. The
	// hourly check calls this on every failure, not only the first.
	dir := t.TempDir()

	for range 2 {
		if err := RemoveAuthConfig(dir); err != nil {
			t.Errorf("RemoveAuthConfig() err = %v, want nil", err)
		}
	}
}

// ///////////////////////////////////////////////
// AuthConfigPath
// ///////////////////////////////////////////////

func TestAuthConfigPath_AnswersBeforeAnythingIsWritten(t *testing.T) {
	// The recorder learns where the file will be at construction, before the
	// operator authenticates at all.
	dir := t.TempDir()

	if got, want := AuthConfigPath(dir), filepath.Join(dir, AuthConfigName); got != want {
		t.Errorf("AuthConfigPath() = %q, want %q", got, want)
	}
}

// ///////////////////////////////////////////////
// ReadAuthConfig
// ///////////////////////////////////////////////

func TestReadAuthConfig_RecoversWhatWasWritten(t *testing.T) {
	// The daemon's only route to the token: it runs under an S4U logon that
	// cannot open the credential store.
	dir := t.TempDir()
	if _, err := WriteAuthConfig(dir, sentinelToken); err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}

	got, err := ReadAuthConfig(dir)
	if err != nil {
		t.Fatalf("ReadAuthConfig() err = %v, want nil", err)
	}
	if got != sentinelToken {
		t.Errorf("ReadAuthConfig() = %q, want the written token", got)
	}
}

func TestReadAuthConfig_ReportsAnAbsentFileWithoutNamingAToken(t *testing.T) {
	// Logout removes the file deliberately, so this path runs in ordinary
	// operation and its error reaches the daemon log.
	_, err := ReadAuthConfig(t.TempDir())

	if err == nil {
		t.Fatal("ReadAuthConfig() err = nil for an absent file, want a failure")
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("the error carries a token: %q", err.Error())
	}
}

func TestReadAuthConfig_RefusesAFileWithNoTokenInIt(t *testing.T) {
	// A hand-edited or truncated file must not yield an empty token that
	// then gets sent to Twitch as a malformed header on every capture.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AuthConfigName),
		[]byte("# a comment and nothing else\n"), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	if _, err := ReadAuthConfig(dir); err == nil {
		t.Error("ReadAuthConfig() err = nil for a file with no token, want a failure")
	}
}

func TestReadAuthConfig_SurvivesTheOperatorsOwnAdditions(t *testing.T) {
	// The file is this project's to rewrite, but a reader that only
	// accepted a single-line file would break the moment anyone added one.
	dir := t.TempDir()
	body := "# stream-dvr\ntwitch-low-latency=true\n" +
		"twitch-api-header=Authorization=OAuth " + sentinelToken + "\n"
	if err := os.WriteFile(filepath.Join(dir, AuthConfigName), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	got, err := ReadAuthConfig(dir)
	if err != nil {
		t.Fatalf("ReadAuthConfig() err = %v, want nil", err)
	}
	if got != sentinelToken {
		t.Errorf("ReadAuthConfig() = %q, want the token", got)
	}
}
