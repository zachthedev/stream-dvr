package notify

import (
	"strings"
	"testing"
)

func TestNotifyScript_TakesItsTextFromArgv(t *testing.T) {
	// This is the whole reason macOS is safe. A title carrying a quote or a
	// backslash would become script the operator did not write, and every
	// stream title is text a stranger chose. The script must therefore have
	// no place for text to be substituted into it.
	if strings.ContainsAny(notifyScript, "%") {
		t.Errorf("the script carries a format verb, so text can be interpolated into it:\n%s", notifyScript)
	}
	for _, want := range []string{"item 1 of argv", "item 2 of argv"} {
		if !strings.Contains(notifyScript, want) {
			t.Errorf("the script does not read %s, so its text comes from somewhere else", want)
		}
	}
}

func TestDesktopAvailable_NeedsOsascript(t *testing.T) {
	// The recorder is a launchd agent in the gui domain, so it is inside
	// the session whenever it runs at all. osascript ships with macOS, so a
	// refusal here means somebody took the system apart.
	if err := desktopAvailable(); err != nil {
		t.Logf("osascript is not on PATH on this machine: %v", err)
	}
}
