package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Test helpers
// ///////////////////////////////////////////////

// agentConfig writes a config naming a library and returns its path.
//
// The agent reads two things and no more: whether desktop notifications are
// wanted, and which library's socket to follow.
func agentConfig(t *testing.T, desktop bool) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "recordings")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating a library root: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[library]\nroot = " + tomlString(root) + "\n\n[notify]\ndesktop = "
	if desktop {
		body += "true\n"
	} else {
		body += "false\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing a config: %v", err)
	}
	return path
}

// tomlString renders a path as a TOML literal string, which on Windows is
// full of backslashes a basic string would read as escapes.
func tomlString(path string) string {
	return "'" + path + "'"
}

// ///////////////////////////////////////////////
// runNotifyAgent
// ///////////////////////////////////////////////

func TestRunNotifyAgent_RefusesWhenDesktopIsOff(t *testing.T) {
	// The agent and the recorder read one setting. An operator who turned
	// notifications off and still has an agent running from an old Run key
	// is told why it stopped rather than left with a process doing nothing.
	var out bytes.Buffer
	err := runNotifyAgent(context.Background(), &out, agentConfig(t, false))

	if err == nil {
		t.Fatal("runNotifyAgent() err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "turned off") {
		t.Errorf("runNotifyAgent() err = %v, want it to name the setting", err)
	}
}

func TestRunNotifyAgent_ReportsAConfigItCannotRead(t *testing.T) {
	// Started by the session with no terminal to complain to, so the
	// failure has to be an error the caller reports rather than a silence.
	var out bytes.Buffer
	err := runNotifyAgent(context.Background(), &out,
		filepath.Join(t.TempDir(), "absent.toml"))

	if err == nil {
		t.Error("runNotifyAgent() err = nil, want the unreadable config reported")
	}
}
