//go:build darwin

package service

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// plistNode decodes a property list far enough to look a key up.
//
// A dict is a flat run of alternating key and value elements, so document
// order rather than nesting carries the association. Decoding it generically
// asserts against the document launchd will parse rather than against the
// struct that wrote it.
type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

// launchdFixture returns a manager writing agents into a temp directory.
func launchdFixture(t *testing.T) launchdManager {
	t.Helper()

	return launchdManager{
		agentDir:   filepath.Join(t.TempDir(), "LaunchAgents"),
		domain:     "gui/501",
		searchPath: searchPath("/Users/operator"),
	}
}

// bootstrapped returns a manager with the agent already on disk, which is
// the state every method other than Install acts on.
func bootstrapped(t *testing.T, name string) launchdManager {
	t.Helper()

	manager := launchdFixture(t)
	if err := os.MkdirAll(manager.agentDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", manager.agentDir, err)
	}
	if err := os.WriteFile(manager.agentPath(name), []byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("writing the agent: %v", err)
	}
	return manager
}

// parsePlist decodes a rendered agent file.
func parsePlist(t *testing.T, document []byte) plistNode {
	t.Helper()

	var root plistNode
	if err := xml.Unmarshal(document, &root); err != nil {
		t.Fatalf("generated property list does not parse: %v\n%s", err, document)
	}
	if root.XMLName.Local != "plist" {
		t.Fatalf("root element = %q, want plist", root.XMLName.Local)
	}
	if len(root.Children) != 1 || root.Children[0].XMLName.Local != "dict" {
		t.Fatalf("plist does not hold one dict:\n%s", document)
	}
	return root.Children[0]
}

// lookup returns the value element following a key in a dict.
func lookup(t *testing.T, dict plistNode, key string) plistNode {
	t.Helper()

	for i, child := range dict.Children {
		if child.XMLName.Local != "key" || strings.TrimSpace(child.Text) != key {
			continue
		}
		if i+1 >= len(dict.Children) {
			t.Fatalf("key %q has no value", key)
		}
		return dict.Children[i+1]
	}
	t.Fatalf("no %q key in the property list", key)
	return plistNode{}
}

// ///////////////////////////////////////////////
// Agent file
// ///////////////////////////////////////////////

func TestLaunchdManager_AgentFile(t *testing.T) {
	document, err := launchdFixture(t).agentFile(definition())
	if err != nil {
		t.Fatalf("agentFile() err = %v, want nil", err)
	}
	dict := parsePlist(t, document)

	t.Run("names the job", func(t *testing.T) {
		// The label is what every launchctl subcommand addresses, so a
		// mismatch means install succeeds and nothing else finds the job.
		if got := strings.TrimSpace(lookup(t, dict, "Label").Text); got != definition().Name {
			t.Errorf("Label = %q, want %q", got, definition().Name)
		}
	})

	t.Run("starts as soon as it is loaded", func(t *testing.T) {
		if got := lookup(t, dict, "RunAtLoad").XMLName.Local; got != "true" {
			t.Errorf("RunAtLoad = <%s/>, want <true/>", got)
		}
	})

	t.Run("starts again after a crash", func(t *testing.T) {
		// Without it a recorder that dies stays dead until somebody signs
		// in and notices, which is the whole failure a DVR cannot have.
		if got := lookup(t, dict, "KeepAlive").XMLName.Local; got != "true" {
			t.Errorf("KeepAlive = <%s/>, want <true/>", got)
		}
	})

	t.Run("carries the command and arguments", func(t *testing.T) {
		arguments := lookup(t, dict, "ProgramArguments")
		if arguments.XMLName.Local != "array" {
			t.Fatalf("ProgramArguments is <%s>, want an array", arguments.XMLName.Local)
		}

		want := append([]string{definition().Executable}, definition().Args...)
		if len(arguments.Children) != len(want) {
			t.Fatalf("ProgramArguments has %d entries, want %d", len(arguments.Children), len(want))
		}
		for i, entry := range arguments.Children {
			if entry.Text != want[i] {
				t.Errorf("ProgramArguments[%d] = %q, want %q", i, entry.Text, want[i])
			}
		}
	})

	t.Run("gives a graceful shutdown time to finish", func(t *testing.T) {
		// The default is 20 seconds, which kills a remux of a multi-gigabyte
		// recording halfway through.
		if got := strings.TrimSpace(lookup(t, dict, "ExitTimeOut").Text); got != "300" {
			t.Errorf("ExitTimeOut = %q, want 300", got)
		}
	})

	t.Run("paces the restarts", func(t *testing.T) {
		// launchd has no burst limit, so the interval is the only thing
		// standing between a config error and a permanent restart loop.
		if got := strings.TrimSpace(lookup(t, dict, "ThrottleInterval").Text); got != "30" {
			t.Errorf("ThrottleInterval = %q, want 30", got)
		}
	})

	t.Run("is not throttled as a background job", func(t *testing.T) {
		// launchd throttles the CPU and disk of a Background job, which a
		// live capture writing a stream to disk cannot absorb.
		if got := strings.TrimSpace(lookup(t, dict, "ProcessType").Text); got != "Standard" {
			t.Errorf("ProcessType = %q, want Standard", got)
		}
	})

	t.Run("sets a PATH the tools are actually on", func(t *testing.T) {
		// doctor resolves streamlink from a login shell and the agent,
		// running with launchd's own PATH, then reports it missing.
		environment := lookup(t, dict, "EnvironmentVariables")
		if environment.XMLName.Local != "dict" {
			t.Fatalf("EnvironmentVariables is <%s>, want a dict", environment.XMLName.Local)
		}

		path := lookup(t, environment, "PATH").Text
		for _, want := range []string{
			"/Users/operator/.local/bin",
			"/opt/homebrew/bin",
			"/usr/local/bin",
			"/usr/bin",
		} {
			if !strings.Contains(path, want) {
				t.Errorf("PATH missing %q: %s", want, path)
			}
		}
	})
}

func TestLaunchdManager_AgentFileEscapesMarkup(t *testing.T) {
	// A library path carrying markup must not produce a document launchd
	// refuses to parse, because the agent then never loads and the operator
	// is told the install succeeded.
	def := definition()
	def.Executable = "/opt/a & b/<stream-dvr>"
	def.Args = []string{"serve", `--config=/opt/"x"/config.toml`}

	document, err := launchdFixture(t).agentFile(def)
	if err != nil {
		t.Fatalf("agentFile() err = %v, want nil", err)
	}

	arguments := lookup(t, parsePlist(t, document), "ProgramArguments")
	want := append([]string{def.Executable}, def.Args...)
	if len(arguments.Children) != len(want) {
		t.Fatalf("ProgramArguments has %d entries, want %d", len(arguments.Children), len(want))
	}
	for i, entry := range arguments.Children {
		if entry.Text != want[i] {
			t.Errorf("ProgramArguments[%d] = %q, want %q round-tripped", i, entry.Text, want[i])
		}
	}
}

func TestLaunchdManager_AgentPath(t *testing.T) {
	manager := launchdFixture(t)

	got := manager.agentPath("stream-dvr")
	if filepath.Base(got) != "stream-dvr.plist" {
		t.Errorf("agentPath() = %q, want it to end in .plist", got)
	}
	if !strings.HasPrefix(got, manager.agentDir) {
		t.Errorf("agentPath() = %q, want it under %q", got, manager.agentDir)
	}
}

// ///////////////////////////////////////////////
// Install
// ///////////////////////////////////////////////

func TestLaunchdManager_Install(t *testing.T) {
	calls := recordExec(t, "ok")
	manager := launchdFixture(t)
	def := definition()

	if err := manager.Install(def); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}

	t.Run("writes the agent", func(t *testing.T) {
		document, err := os.ReadFile(manager.agentPath(def.Name))
		if err != nil {
			t.Fatalf("reading the agent: %v", err)
		}
		if !strings.Contains(string(document), def.Executable) {
			t.Errorf("agent does not name the binary:\n%s", document)
		}
	})

	t.Run("clears any earlier registration", func(t *testing.T) {
		// launchd refuses to bootstrap a label already in the domain, so a
		// reinstall that skipped this would fail on every machine that had
		// the recorder before.
		if !calls.ran("launchctl", "bootout", "gui/501/stream-dvr") {
			t.Errorf("calls = %v, want the old job booted out", calls.calls)
		}
	})

	t.Run("bootstraps the agent into the user's domain", func(t *testing.T) {
		if !calls.ran("launchctl", "bootstrap", "gui/501", manager.agentPath(def.Name)) {
			t.Errorf("calls = %v, want a bootstrap into gui/501", calls.calls)
		}
	})

	t.Run("keeps the agent to the account", func(t *testing.T) {
		// The agent names the config file and the library root, and only the
		// user's own launchd ever reads it.
		info, err := os.Stat(manager.agentPath(def.Name))
		if err != nil {
			t.Fatalf("Stat() err = %v", err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("agent mode = %o, want nothing for group or other", perm)
		}
	})
}

func TestLaunchdManager_InstallLeavesNoAgentBehindWhenItFails(t *testing.T) {
	// Status keys off the agent file's existence, so one left behind by a
	// failed install reports the recorder as installed forever while nothing
	// ever starts it.
	fakeExec(t, "broken")
	manager := launchdFixture(t)

	if err := manager.Install(definition()); err == nil {
		t.Fatal("Install() err = nil, want the failure reported")
	}
	if _, err := os.Stat(manager.agentPath(definition().Name)); !os.IsNotExist(err) {
		t.Errorf("Stat() err = %v, want the agent removed", err)
	}
}

func TestLaunchdManager_InstallRejectsAnIncompleteDefinition(t *testing.T) {
	if err := launchdFixture(t).Install(Definition{}); err == nil {
		t.Error("Install() err = nil, want a rejection")
	}
}

// ///////////////////////////////////////////////
// Status
// ///////////////////////////////////////////////

func TestLaunchdManager_Status(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    State
		wantErr bool
	}{
		{name: "running", mode: "job-running", want: StateRunning},
		{name: "loaded and idle", mode: "job-waiting", want: StateInstalled},
		{
			// An agent file with no job in the domain is what every machine
			// reports between logins, not a missing registration.
			name: "not loaded",
			mode: "job-unloaded",
			want: StateInstalled,
		},
		{
			// A query that could not be answered is not an answer. Reporting
			// it as absent would tell an operator the recorder is
			// unregistered when it may be recording.
			name:    "launchd unreachable",
			mode:    "broken",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, tt.mode)

			got, err := bootstrapped(t, "stream-dvr").Status("stream-dvr")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Status() err = nil, want the failed query reported")
				}
				if got.State != "" {
					t.Errorf("State = %q, want no claim about a query that failed", got.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status() err = %v, want nil", err)
			}
			if got.State != tt.want {
				t.Errorf("State = %q, want %q", got.State, tt.want)
			}
		})
	}
}

func TestLaunchdManager_StatusAbsentWithoutAnAgent(t *testing.T) {
	// Reporting absent must not require launchd to answer, because the
	// common case is a machine where nothing has been installed yet.
	refuseExec(t)

	got, err := launchdFixture(t).Status("stream-dvr")
	if err != nil {
		t.Fatalf("Status() err = %v, want nil", err)
	}
	if got.State != StateAbsent {
		t.Errorf("State = %q, want %q", got.State, StateAbsent)
	}
}

func TestPrintState(t *testing.T) {
	// The block prints the agent path and the whole argument vector beside
	// the state, so a word found anywhere in it is not an answer.
	tests := []struct {
		name   string
		output string
		want   State
	}{
		{name: "running", output: launchdPrint("running"), want: StateRunning},
		{name: "waiting", output: launchdPrint("waiting"), want: StateInstalled},
		{name: "not running", output: launchdPrint("not running"), want: StateInstalled},
		{
			name:   "a library named for the word",
			output: "gui/501/stream-dvr = {\n\tpath = /Volumes/running/x.plist\n\tstate = waiting\n}\n",
			want:   StateInstalled,
		},
		{
			name:   "an argument named for the word",
			output: "gui/501/stream-dvr = {\n\targuments = {\n\t\t--library=/running\n\t}\n\tstate = waiting\n}\n",
			want:   StateInstalled,
		},
		{name: "no state field", output: "gui/501/stream-dvr = {\n}\n", want: StateInstalled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := printState([]byte(tt.output)); got != tt.want {
				t.Errorf("printState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsNotLoaded(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "no such service",
			output: `Could not find service "stream-dvr" in domain for gui`,
			want:   true,
		},
		{name: "no such process", output: "Boot-out failed: 3: No such process", want: true},
		{
			// A refusal that is not about the job being absent has to
			// surface, or an operator is told a running recorder is stopped.
			name:   "a refusal about something else",
			output: "Bootstrap failed: 5: Input/output error",
			want:   false,
		},
		{name: "no output", output: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotLoaded([]byte(tt.output)); got != tt.want {
				t.Errorf("isNotLoaded(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Uninstall
// ///////////////////////////////////////////////

func TestLaunchdManager_Uninstall(t *testing.T) {
	calls := recordExec(t, "ok")
	manager := bootstrapped(t, "stream-dvr")

	if err := manager.Uninstall("stream-dvr"); err != nil {
		t.Fatalf("Uninstall() err = %v, want nil", err)
	}

	if !calls.ran("launchctl", "bootout", "gui/501/stream-dvr") {
		t.Errorf("calls = %v, want the job booted out", calls.calls)
	}
	if _, err := os.Stat(manager.agentPath("stream-dvr")); !os.IsNotExist(err) {
		t.Errorf("Stat() err = %v, want the agent removed", err)
	}
}

func TestLaunchdManager_UninstallAcceptsAJobThatWasNotLoaded(t *testing.T) {
	// Between logins the agent file is on disk and no job is in the domain.
	// Reporting "it may still be running" there is a lie about a recorder
	// that was never up.
	fakeExec(t, "job-unloaded")
	manager := bootstrapped(t, "stream-dvr")

	if err := manager.Uninstall("stream-dvr"); err != nil {
		t.Errorf("Uninstall() err = %v, want nil for a job that was not loaded", err)
	}
	if _, err := os.Stat(manager.agentPath("stream-dvr")); !os.IsNotExist(err) {
		t.Errorf("Stat() err = %v, want the agent removed", err)
	}
}

func TestLaunchdManager_UninstallReportsARecorderItCouldNotStop(t *testing.T) {
	// The registration is gone from disk either way. If the recorder is
	// still up then nothing on disk points at it any more, which is the one
	// state an operator has to be told about.
	fakeExec(t, "broken")
	manager := bootstrapped(t, "stream-dvr")

	if err := manager.Uninstall("stream-dvr"); err == nil {
		t.Fatal("Uninstall() err = nil, want the failure reported")
	}
	if _, err := os.Stat(manager.agentPath("stream-dvr")); !os.IsNotExist(err) {
		t.Errorf("Stat() err = %v, want the agent removed anyway", err)
	}
}

func TestLaunchdManager_UninstallIsSafeToRepeat(t *testing.T) {
	// Removing a registration that is not there is the desired end state.
	refuseExec(t)

	if err := launchdFixture(t).Uninstall("stream-dvr"); err != nil {
		t.Errorf("Uninstall() err = %v, want nil for an absent agent", err)
	}
}

// ///////////////////////////////////////////////
// Start and Stop
// ///////////////////////////////////////////////

func TestLaunchdManager_StartAndStop(t *testing.T) {
	t.Run("start loads the job and kicks it", func(t *testing.T) {
		// kickstart reaches a job that is in the domain and bootstrap puts
		// one there, so doing both starts the recorder from either state.
		calls := recordExec(t, "ok")
		manager := bootstrapped(t, "stream-dvr")

		if err := manager.Start("stream-dvr"); err != nil {
			t.Fatalf("Start() err = %v, want nil", err)
		}
		if !calls.ran("launchctl", "bootstrap", "gui/501", manager.agentPath("stream-dvr")) {
			t.Errorf("calls = %v, want a bootstrap", calls.calls)
		}
		if !calls.ran("launchctl", "kickstart", "gui/501/stream-dvr") {
			t.Errorf("calls = %v, want a kickstart", calls.calls)
		}
	})

	t.Run("start carries launchd's own message", func(t *testing.T) {
		fakeExec(t, "broken")

		err := launchdFixture(t).Start("stream-dvr")
		if err == nil {
			t.Fatal("Start() err = nil, want the failure reported")
		}
		if !strings.Contains(err.Error(), "something else went wrong") {
			t.Errorf("Start() err = %q, want it to quote launchd", err)
		}
	})

	t.Run("stop unloads the job", func(t *testing.T) {
		// The agent file survives, so launchd loads the recorder again at
		// the next login. Removing it for good is Uninstall's job.
		calls := recordExec(t, "ok")
		manager := bootstrapped(t, "stream-dvr")

		if err := manager.Stop("stream-dvr"); err != nil {
			t.Fatalf("Stop() err = %v, want nil", err)
		}
		if !calls.ran("launchctl", "bootout", "gui/501/stream-dvr") {
			t.Errorf("calls = %v, want the job booted out", calls.calls)
		}
		if _, err := os.Stat(manager.agentPath("stream-dvr")); err != nil {
			t.Errorf("Stat() err = %v, want the agent left in place", err)
		}
	})

	t.Run("stopping a stopped recorder is not a failure", func(t *testing.T) {
		fakeExec(t, "job-unloaded")

		if err := launchdFixture(t).Stop("stream-dvr"); err != nil {
			t.Errorf("Stop() err = %v, want nil for a job that was not loaded", err)
		}
	})

	t.Run("stop carries launchd's own message", func(t *testing.T) {
		fakeExec(t, "broken")

		err := launchdFixture(t).Stop("stream-dvr")
		if err == nil {
			t.Fatal("Stop() err = nil, want the failure reported")
		}
		if !strings.Contains(err.Error(), "something else went wrong") {
			t.Errorf("Stop() err = %q, want it to quote launchd", err)
		}
	})

	t.Run("both reject an empty name", func(t *testing.T) {
		refuseExec(t)
		manager := launchdFixture(t)

		if err := manager.Start(""); err == nil {
			t.Error("Start() err = nil, want a rejection")
		}
		if err := manager.Stop(""); err == nil {
			t.Error("Stop() err = nil, want a rejection")
		}
	})
}

func TestLaunchdManager_RejectsAnEmptyName(t *testing.T) {
	refuseExec(t)
	manager := launchdFixture(t)

	if err := manager.Uninstall(""); err == nil {
		t.Error("Uninstall() err = nil, want a rejection")
	}
	if _, err := manager.Status(""); err == nil {
		t.Error("Status() err = nil, want a rejection")
	}
}

func TestLaunchdManager_Mechanism(t *testing.T) {
	if launchdFixture(t).Mechanism() == "" {
		t.Error("Mechanism() is empty, want it to name the platform facility")
	}
}

// ///////////////////////////////////////////////
// Platform selection
// ///////////////////////////////////////////////

func TestNewManager(t *testing.T) {
	manager, err := newManager()
	if err != nil {
		t.Fatalf("newManager() err = %v, want nil", err)
	}

	launchd, ok := manager.(launchdManager)
	if !ok {
		t.Fatalf("newManager() = %T, want a launchd manager", manager)
	}

	t.Run("writes into the user's LaunchAgents", func(t *testing.T) {
		// A daemon in /Library runs as root and cannot read the streamlink
		// credentials in the operator's home directory.
		if filepath.Base(launchd.agentDir) != "LaunchAgents" {
			t.Errorf("agentDir = %q, want the user's LaunchAgents", launchd.agentDir)
		}
	})

	t.Run("targets the user's own domain", func(t *testing.T) {
		if !strings.HasPrefix(launchd.domain, "gui/") {
			t.Errorf("domain = %q, want a gui/<uid> target", launchd.domain)
		}
	})

	t.Run("carries the tool directories", func(t *testing.T) {
		if launchd.searchPath == "" {
			t.Error("searchPath is empty, want the tool directories")
		}
	})
}
