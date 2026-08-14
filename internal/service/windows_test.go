//go:build windows

package service

import (
	"encoding/xml"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// unmarshalTask parses a rendered task body.
func unmarshalTask(t *testing.T, document string) taskXML {
	t.Helper()

	var parsed taskXML
	if err := xml.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("generated XML does not parse: %v", err)
	}
	return parsed
}

// throughBSTR returns what RegisterTask receives for a string argument.
//
// go-ole allocates one with SysAllocStringLen, which runs utf16.Encode over
// the value's runes, so this is the conversion every document crosses on
// its way to the scheduler.
func throughBSTR(document string) string {
	return string(utf16.Decode(utf16.Encode([]rune(document))))
}

// ///////////////////////////////////////////////
// Task XML
// ///////////////////////////////////////////////

func TestBuildTaskXML(t *testing.T) {
	document, err := buildTaskXML(definition(), `EXAMPLE-PC\operator`)
	if err != nil {
		t.Fatalf("buildTaskXML() err = %v, want nil", err)
	}

	parsed := unmarshalTask(t, document)

	t.Run("runs as the operator without a stored password", func(t *testing.T) {
		// S4U is the whole reason the document carries a principal at all:
		// it is the only way to run as the user before anyone signs in.
		if parsed.Principals.Principal.LogonType != "S4U" {
			t.Errorf("LogonType = %q, want S4U", parsed.Principals.Principal.LogonType)
		}
		if parsed.Principals.Principal.UserID != `EXAMPLE-PC\operator` {
			t.Errorf("UserId = %q, want the invoking user", parsed.Principals.Principal.UserID)
		}
		if parsed.Principals.Principal.RunLevel != "LeastPrivilege" {
			t.Errorf("RunLevel = %q, want LeastPrivilege", parsed.Principals.Principal.RunLevel)
		}
	})

	t.Run("starts after a reboot with nobody signed in", func(t *testing.T) {
		if !parsed.Triggers.Boot.Enabled {
			t.Error("BootTrigger is disabled, want a recorder that survives a reboot")
		}
	})

	t.Run("has no execution time limit", func(t *testing.T) {
		// The scheduler's default is three days, which would kill a
		// recorder that has been up since the last reboot.
		if parsed.Settings.ExecutionTimeLimit != "PT0S" {
			t.Errorf("ExecutionTimeLimit = %q, want PT0S", parsed.Settings.ExecutionTimeLimit)
		}
	})

	t.Run("carries the command and arguments", func(t *testing.T) {
		if parsed.Actions.Exec.Command != definition().Executable {
			t.Errorf("Command = %q, want %q", parsed.Actions.Exec.Command, definition().Executable)
		}
		if parsed.Actions.Exec.Arguments != "serve" {
			t.Errorf("Arguments = %q, want %q", parsed.Actions.Exec.Arguments, "serve")
		}
	})

	t.Run("keeps running on battery", func(t *testing.T) {
		// A laptop that unplugs mid-broadcast must keep recording.
		if parsed.Settings.DisallowStartIfOnBatteries {
			t.Error("DisallowStartIfOnBatteries is set, want recording on battery")
		}
		if parsed.Settings.StopIfGoingOnBatteries {
			t.Error("StopIfGoingOnBatteries is set, want recording to continue")
		}
	})
}

func TestBuildTaskXML_EscapesMarkup(t *testing.T) {
	// A description or path carrying markup must not produce a document
	// the scheduler rejects, or refuses to import as written.
	def := definition()
	def.Description = `Records "live" streams & <things>`
	def.Executable = `C:\dir & co\stream-dvr.exe`

	document, err := buildTaskXML(def, `DOMAIN\User`)
	if err != nil {
		t.Fatalf("buildTaskXML() err = %v, want nil", err)
	}

	parsed := unmarshalTask(t, document)
	if parsed.Registration.Description != def.Description {
		t.Errorf("Description = %q, want it round-tripped", parsed.Registration.Description)
	}
	if parsed.Actions.Exec.Command != def.Executable {
		t.Errorf("Command = %q, want it round-tripped", parsed.Actions.Exec.Command)
	}
}

func TestBuildTaskXML_CarriesNoDeclaration(t *testing.T) {
	// RegisterTask takes the document as a wide string, so it already knows
	// the encoding. A declaration naming one contradicts the string it
	// arrived in, and the scheduler answers "unable to switch the
	// encoding", which points nowhere near the cause.
	document, err := buildTaskXML(definition(), `DOMAIN\User`)
	if err != nil {
		t.Fatalf("buildTaskXML() err = %v, want nil", err)
	}

	if strings.HasPrefix(strings.TrimSpace(document), "<?xml") {
		header, _, _ := strings.Cut(document, "\n")
		t.Errorf("document starts with %q, want the body alone", header)
	}
}

// ///////////////////////////////////////////////
// What the scheduler is handed
// ///////////////////////////////////////////////

func TestInstall_HandsTheSchedulerADocumentABSTRCanCarry(t *testing.T) {
	// The document travels to RegisterTask as a wide string, so whatever
	// Install passes is converted rune by rune. Bytes handed over as a
	// string arrive as one character per byte, with every UTF-16 pad byte
	// its own NUL and the byte order mark two replacement runes, and the
	// scheduler refuses the result as malformed XML.
	sched := &fakeScheduler{}

	if err := (taskManager{sched: sched}).Install(validDefinition()); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}
	if len(sched.documents) != 1 {
		t.Fatalf("registered %d documents, want 1", len(sched.documents))
	}

	document := sched.documents[0]
	received := throughBSTR(document)
	if received != document {
		t.Fatalf("the scheduler receives %q, want the document Install rendered",
			received[:min(80, len(received))])
	}

	parsed := unmarshalTask(t, received)
	if parsed.Principals.Principal.LogonType != "S4U" {
		t.Errorf("LogonType = %q, want S4U", parsed.Principals.Principal.LogonType)
	}
}

func TestInstall_CarriesNonASCIIToTheScheduler(t *testing.T) {
	// A description or install path outside ASCII must reach the registered
	// task as itself rather than as mojibake.
	const source = "配信 ✨"

	def := validDefinition()
	def.Description = source
	sched := &fakeScheduler{}

	if err := (taskManager{sched: sched}).Install(def); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}
	if len(sched.documents) != 1 {
		t.Fatalf("registered %d documents, want 1", len(sched.documents))
	}

	parsed := unmarshalTask(t, throughBSTR(sched.documents[0]))
	if parsed.Registration.Description != source {
		t.Errorf("Description = %q, want %q", parsed.Registration.Description, source)
	}
}

func TestCommandLine_SurvivesTheRoundTrip(t *testing.T) {
	// The scheduler stores one Arguments string and Windows splits it again
	// at launch, so an argument that does not survive that round trip
	// reaches the recorder as something else.
	tests := []struct {
		name string
		args []string
	}{
		{name: "plain", args: []string{"serve"}},
		{
			name: "a path with a space",
			args: []string{"serve", "--config", `C:\Program Files\stream-dvr\config.toml`},
		},
		{name: "a quote", args: []string{"serve", `--config`, `C:\a"b\config.toml`}},
		{name: "trailing backslash", args: []string{"serve", "--config", `C:\dir\`}},
		{name: "an argument that looks like a flag", args: []string{"serve", "--config", "--foreground"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Through the whole document rather than through commandLine
			// alone, because the defect worth catching is the task carrying
			// an unquoted string, not the helper being wrong.
			def := definition()
			def.Args = tt.args

			document, err := buildTaskXML(def, `DOMAIN\User`)
			if err != nil {
				t.Fatalf("buildTaskXML() err = %v, want nil", err)
			}
			rendered := unmarshalTask(t, document).Actions.Exec.Arguments

			got, err := windows.DecomposeCommandLine("stream-dvr.exe " + rendered)
			if err != nil {
				t.Fatalf("DecomposeCommandLine(%q) err = %v", rendered, err)
			}
			got = got[1:]

			if len(got) != len(tt.args) {
				t.Fatalf("argv = %q, want %q (rendered %q)", got, tt.args, rendered)
			}
			for i := range tt.args {
				if got[i] != tt.args[i] {
					t.Errorf("argv[%d] = %q, want %q (rendered %q)", i, got[i], tt.args[i], rendered)
				}
			}
		})
	}
}

func TestCurrentUser_ComesFromTheProcessToken(t *testing.T) {
	// The result names the principal an elevated install writes into the
	// task. HKCU\Environment is writable at medium integrity and propagates
	// into a shell elevated from that account, so an identity read from the
	// environment would let an unprivileged process choose it.
	want, err := currentUser()
	if err != nil {
		t.Fatalf("currentUser() err = %v, want nil", err)
	}
	if want == "" {
		t.Fatal("currentUser() = \"\", want the account name")
	}

	for _, pair := range []struct{ domain, user string }{
		{domain: "NT AUTHORITY", user: "SYSTEM"},
		{domain: "BUILTIN", user: "Administrators"},
		{domain: "", user: ""},
	} {
		t.Setenv("USERDOMAIN", pair.domain)
		t.Setenv("USERNAME", pair.user)

		got, err := currentUser()
		if err != nil {
			t.Fatalf("currentUser() err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("currentUser() = %q with USERDOMAIN=%q USERNAME=%q, want %q unchanged",
				got, pair.domain, pair.user, want)
		}
	}
}
