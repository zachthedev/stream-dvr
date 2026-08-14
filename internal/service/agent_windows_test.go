package service

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// ///////////////////////////////////////////////
// Test helpers
// ///////////////////////////////////////////////

// testAutostart returns a registration aimed at a throwaway key.
//
// Never the real Run key: a test that wrote there would register the
// notify agent on the machine running the tests, and Windows would start
// it at the next logon.
func testAutostart(t *testing.T) runKeyAutostart {
	t.Helper()

	// One level under Software, so deleting it leaves nothing behind.
	// DeleteKey removes the key named and not its parent, so a nested path
	// would leave an empty parent in the operator's registry after every
	// run.
	path := `Software\stream-dvr-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() {
		registry.DeleteKey(registry.CURRENT_USER, path) // a key never created is nothing to clean up
	})

	return runKeyAutostart{root: registry.CURRENT_USER, path: path, value: runKeyValue}
}

// ///////////////////////////////////////////////
// Install
// ///////////////////////////////////////////////

func TestRunKeyAutostart_InstallRegistersTheCommand(t *testing.T) {
	autostart := testAutostart(t)

	if err := autostart.Install(`C:\tools\stream-dvr.exe`, []string{"notify-agent"}); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, autostart.path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("opening the test key: %v", err)
	}
	defer key.Close()

	got, _, err := key.GetStringValue(runKeyValue)
	if err != nil {
		t.Fatalf("reading the registered value: %v", err)
	}
	if want := `"C:\tools\stream-dvr.exe" notify-agent`; got != want {
		t.Errorf("registered %q, want %q", got, want)
	}
}

func TestRunKeyAutostart_InstallReplacesAnEarlierRegistration(t *testing.T) {
	// An operator who moves the binary and installs again must end up with
	// one entry naming the new path, not two entries racing at logon.
	autostart := testAutostart(t)

	if err := autostart.Install(`C:\old\stream-dvr.exe`, []string{"notify-agent"}); err != nil {
		t.Fatalf("first Install() err = %v, want nil", err)
	}
	if err := autostart.Install(`C:\new\stream-dvr.exe`, []string{"notify-agent"}); err != nil {
		t.Fatalf("second Install() err = %v, want nil", err)
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, autostart.path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("opening the test key: %v", err)
	}
	defer key.Close()

	got, _, err := key.GetStringValue(runKeyValue)
	if err != nil {
		t.Fatalf("reading the registered value: %v", err)
	}
	if !strings.Contains(got, `C:\new\`) {
		t.Errorf("registered %q, want the newer path", got)
	}
}

func TestRunKeyAutostart_InstallRefusesAnEmptyExecutable(t *testing.T) {
	// An entry with no program is one Windows tries and fails to run at
	// every logon, reporting nothing anybody sees.
	autostart := testAutostart(t)

	if err := autostart.Install("   ", []string{"notify-agent"}); err == nil {
		t.Error("Install() err = nil, want a refusal for an empty executable")
	}
}

// ///////////////////////////////////////////////
// Uninstall
// ///////////////////////////////////////////////

func TestRunKeyAutostart_UninstallRemovesTheEntry(t *testing.T) {
	autostart := testAutostart(t)

	if err := autostart.Install(`C:\tools\stream-dvr.exe`, []string{"notify-agent"}); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}
	if err := autostart.Uninstall(); err != nil {
		t.Fatalf("Uninstall() err = %v, want nil", err)
	}

	installed, err := autostart.Installed()
	if err != nil {
		t.Fatalf("Installed() err = %v, want nil", err)
	}
	if installed {
		t.Error("Installed() = true after Uninstall, want false")
	}
}

func TestRunKeyAutostart_UninstallIsRepeatable(t *testing.T) {
	// uninstall runs it, and so does a reinstall. Neither has any way to
	// know whether an entry is there.
	autostart := testAutostart(t)

	if err := autostart.Uninstall(); err != nil {
		t.Errorf("Uninstall() with no key err = %v, want nil", err)
	}
	if err := autostart.Install(`C:\tools\stream-dvr.exe`, nil); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}
	if err := autostart.Uninstall(); err != nil {
		t.Fatalf("first Uninstall() err = %v, want nil", err)
	}
	if err := autostart.Uninstall(); err != nil {
		t.Errorf("second Uninstall() err = %v, want nil", err)
	}
}

// ///////////////////////////////////////////////
// Installed
// ///////////////////////////////////////////////

func TestRunKeyAutostart_InstalledReportsAnAbsentKey(t *testing.T) {
	// The ordinary state before the first install, and it must read as
	// "not registered" rather than as a failure.
	autostart := testAutostart(t)

	installed, err := autostart.Installed()
	if err != nil {
		t.Fatalf("Installed() err = %v, want nil", err)
	}
	if installed {
		t.Error("Installed() = true with no key, want false")
	}
}

func TestRunKeyAutostart_InstalledReportsAKeyWithoutTheValue(t *testing.T) {
	// The real Run key always exists and is full of other programs. Only
	// this project's own value counts.
	autostart := testAutostart(t)

	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostart.path, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("creating the test key: %v", err)
	}
	if err := key.SetStringValue("something-else", "another program"); err != nil {
		t.Fatalf("seeding another value: %v", err)
	}
	key.Close()

	installed, err := autostart.Installed()
	if err != nil {
		t.Fatalf("Installed() err = %v, want nil", err)
	}
	if installed {
		t.Error("Installed() = true for another program's value, want false")
	}
}

// ///////////////////////////////////////////////
// runCommand
// ///////////////////////////////////////////////

func TestRunCommand(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		args       []string
		want       string
		why        string
	}{
		{
			name:       "quotes the executable",
			executable: `C:\tools\stream-dvr.exe`,
			args:       []string{"notify-agent"},
			want:       `"C:\tools\stream-dvr.exe" notify-agent`,
			why:        "an unquoted path is read as a program name followed by arguments",
		},
		{
			name:       "quotes a path with a space",
			executable: `C:\Program Files\stream-dvr\stream-dvr.exe`,
			args:       []string{"notify-agent"},
			want:       `"C:\Program Files\stream-dvr\stream-dvr.exe" notify-agent`,
			why:        "the ordinary install location has a space in it",
		},
		{
			name:       "leaves a plain argument alone",
			executable: `C:\tools\stream-dvr.exe`,
			args:       []string{"notify-agent", "--config", `C:\config.toml`},
			want:       `"C:\tools\stream-dvr.exe" notify-agent --config C:\config.toml`,
			why:        "an operator reads this entry, so it stays as plain as it can be",
		},
		{
			name:       "quotes an argument with a space",
			executable: `C:\tools\stream-dvr.exe`,
			args:       []string{"--config", `C:\My Configs\config.toml`},
			want:       `"C:\tools\stream-dvr.exe" --config "C:\My Configs\config.toml"`,
			why:        "a config path under a user's documents has spaces",
		},
		{
			name:       "handles no arguments",
			executable: `C:\tools\stream-dvr.exe`,
			want:       `"C:\tools\stream-dvr.exe"`,
			why:        "the quoting must not depend on there being arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runCommand(tt.executable, tt.args); got != tt.want {
				t.Errorf("runCommand() = %q, want %q (%s)", got, tt.want, tt.why)
			}
		})
	}
}
