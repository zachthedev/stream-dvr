package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Package-level vars
// ///////////////////////////////////////////////

func TestLogs_Valid(t *testing.T) {
	if len(Logs) == 0 {
		t.Fatal("Logs is empty; expected at least one registered log file")
	}
	for _, l := range Logs {
		if l.Name == "" {
			t.Errorf("Log entry has empty Name: %+v", l)
		}
		if l.RelPath == "" {
			t.Errorf("Log entry %q has empty RelPath", l.Name)
		}
	}
}

func TestDataDir(t *testing.T) {
	t.Run("resolves under the binary name", func(t *testing.T) {
		got := DataDir()
		if got == "" {
			t.Fatal("DataDir() returned empty string")
		}
		// The OS config location yields "<config>/stream-dvr". The fallback
		// for an unavailable location yields "<home>/.stream-dvr".
		base := filepath.Base(got)
		if base != BinaryName && base != "."+BinaryName {
			t.Errorf("DataDir() = %q, want last element %q or %q", got, BinaryName, "."+BinaryName)
		}
	})

	t.Run("env override wins", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "explicit")
		t.Setenv(EnvDataDir, want)
		if got := DataDir(); got != want {
			t.Errorf("DataDir() with %s set = %q, want %q", EnvDataDir, got, want)
		}
	})
}

func TestLogDir(t *testing.T) {
	t.Run("resolves under the binary name", func(t *testing.T) {
		got := LogDir()
		if got == "" {
			t.Fatal("LogDir() returned empty string")
		}
		if base := filepath.Base(got); base != BinaryName && base != "."+BinaryName {
			t.Errorf("LogDir() = %q, want last element %q or %q", got, BinaryName, "."+BinaryName)
		}
	})

	t.Run("env override wins", func(t *testing.T) {
		// One variable moves everything machine-local, so an operator who
		// redirects the data directory does not then hunt for the logs.
		want := filepath.Join(t.TempDir(), "explicit")
		t.Setenv(EnvDataDir, want)
		if got := LogDir(); got != want {
			t.Errorf("LogDir() with %s set = %q, want %q", EnvDataDir, got, want)
		}
	})
}

func TestLogRoot(t *testing.T) {
	// Every platform is checked from every host, because a rule that only
	// runs where it applies is a rule nothing checks until someone is on
	// that platform wondering where the logs went.
	const home = "/home/operator"

	tests := []struct {
		name      string
		goos      string
		home      string
		stateHome string
		dataDir   string
		want      string
		why       string
	}{
		{
			name:    "windows keeps logs with the config",
			goos:    "windows",
			home:    `C:\Users\operator`,
			dataDir: `C:\Users\operator\AppData\Roaming\stream-dvr`,
			want:    `C:\Users\operator\AppData\Roaming\stream-dvr`,
			why:     "Windows has one per-application location and no separate one for logs",
		},
		{
			name: "macOS uses the library log directory",
			goos: "darwin",
			home: "/Users/operator",
			want: filepath.Join("/Users/operator", "Library", "Logs", BinaryName),
			why:  "Console reads ~/Library/Logs, and Application Support is not for logs",
		},
		{
			name: "linux follows XDG_STATE_HOME",
			goos: "linux",
			home: home,
			// A log is state that survives a restart, which XDG puts under
			// XDG_STATE_HOME rather than beside the config.
			stateHome: "/home/operator/.local/state",
			want:      filepath.Join("/home/operator/.local/state", BinaryName),
			why:       "an operator who set XDG_STATE_HOME meant it",
		},
		{
			name: "linux defaults to the XDG state location",
			goos: "linux",
			home: home,
			want: filepath.Join(home, ".local", "state", BinaryName),
			why:  "the specification names ~/.local/state when the variable is unset",
		},
		{
			name:      "another unix follows the same rule",
			goos:      "freebsd",
			home:      home,
			stateHome: "/var/state",
			want:      filepath.Join("/var/state", BinaryName),
			why:       "XDG is not Linux-specific and nothing else claims the question",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := logRoot(tt.goos, tt.home, tt.stateHome, tt.dataDir)
			if got != tt.want {
				t.Errorf("logRoot(%q, ...) = %q, want %q (%s)", tt.goos, got, tt.want, tt.why)
			}
		})
	}
}

func TestLogRoot_KeepsLogsOutOfTheConfigDirectory(t *testing.T) {
	// The one thing that must not happen: logs written into the directory a
	// user hand-edits their config in, on a platform whose convention says
	// otherwise.
	tests := []struct {
		goos    string
		home    string
		dataDir string
	}{
		{goos: "linux", home: "/home/operator", dataDir: "/home/operator/.config/stream-dvr"},
		{goos: "darwin", home: "/Users/operator", dataDir: "/Users/operator/Library/Application Support/stream-dvr"},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			if got := logRoot(tt.goos, tt.home, "", tt.dataDir); got == tt.dataDir {
				t.Errorf("logRoot(%q, ...) = %q, want it out of the config directory", tt.goos, got)
			}
		})
	}
}

// ///////////////////////////////////////////////
// RuntimeDir
// ///////////////////////////////////////////////

func TestRuntimeDir(t *testing.T) {
	t.Run("resolves to a directory", func(t *testing.T) {
		if got := RuntimeDir(); got == "" {
			t.Fatal("RuntimeDir() returned empty string")
		}
	})

	t.Run("env override wins", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "explicit")
		t.Setenv(EnvDataDir, want)
		if got := RuntimeDir(); got != want {
			t.Errorf("RuntimeDir() with %s set = %q, want %q", EnvDataDir, got, want)
		}
	})
}

func TestRuntimeRoot(t *testing.T) {
	const dataDir = "/data/stream-dvr"

	tests := []struct {
		name         string
		goos         string
		runtimeHome  string
		localAppData string
		want         string
		why          string
	}{
		{
			name:         "windows uses the local application data directory",
			goos:         "windows",
			localAppData: `C:\Users\operator\AppData\Local`,
			want:         filepath.Join(`C:\Users\operator\AppData\Local`, BinaryName),
			why:          "it is per-user and does not roam, which a socket path must not",
		},
		{
			name: "windows falls back to the data directory",
			goos: "windows",
			want: dataDir,
			why:  "a socket somewhere has to be possible even with the variable unset",
		},
		{
			name: "macOS uses the data directory",
			goos: "darwin",
			want: dataDir,
			why:  "macOS has no per-user runtime directory to use instead",
		},
		{
			name:        "linux follows XDG_RUNTIME_DIR",
			goos:        "linux",
			runtimeHome: "/run/user/1000",
			want:        filepath.Join("/run/user/1000", BinaryName),
			why:         "the session manager empties it at logout, which is a socket's lifetime",
		},
		{
			name: "linux falls back to the data directory",
			goos: "linux",
			want: dataDir,
			why:  "a login without a session manager still has to bind somewhere",
		},
		{
			name:        "another unix follows the same rule",
			goos:        "freebsd",
			runtimeHome: "/var/run/user/1000",
			want:        filepath.Join("/var/run/user/1000", BinaryName),
			why:         "XDG is not Linux-specific and nothing else claims the question",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runtimeRoot(tt.goos, tt.runtimeHome, tt.localAppData, dataDir)
			if got != tt.want {
				t.Errorf("runtimeRoot(%q, ...) = %q, want %q (%s)", tt.goos, got, tt.want, tt.why)
			}
		})
	}
}

// ///////////////////////////////////////////////
// NotifySocket
// ///////////////////////////////////////////////

func TestNotifySocket_SeparatesLibraries(t *testing.T) {
	// Two libraries recorded by one machine must not land on one socket, or
	// each would carry the other's events.
	first := NotifySocket(filepath.Join(t.TempDir(), "first"))
	second := NotifySocket(filepath.Join(t.TempDir(), "second"))

	if first == second {
		t.Errorf("two libraries share the socket %q", first)
	}
}

func TestNotifySocket_IsStableForOneLibrary(t *testing.T) {
	// The daemon and the agent derive this independently. A name that varied
	// between two calls would leave the agent dialing nothing.
	root := t.TempDir()
	if first, second := NotifySocket(root), NotifySocket(root); first != second {
		t.Errorf("NotifySocket is unstable: %q then %q", first, second)
	}
}

func TestNotifySocket_AgreesAcrossSpellingsOfOneRoot(t *testing.T) {
	// An operator writes a root one way in the config and another way on the
	// command line. Both name one directory, so both must name one socket.
	root := t.TempDir()
	sep := string(filepath.Separator)

	// Built by concatenation rather than filepath.Join, which cleans its
	// arguments: a spelling normalized before the call would prove nothing
	// about the call.
	spellings := map[string]string{
		"trailing separator": root + sep,
		"undotted":           root + sep + ".",
		"through a parent":   root + sep + "sub" + sep + "..",
	}
	if runtime.GOOS == "windows" {
		// Windows paths are case-insensitive, so these are one directory.
		spellings["upper case"] = strings.ToUpper(root)
	}

	want := NotifySocket(root)
	for name, spelling := range spellings {
		t.Run(name, func(t *testing.T) {
			if got := NotifySocket(spelling); got != want {
				t.Errorf("NotifySocket(%q) = %q, want %q", spelling, got, want)
			}
		})
	}
}

func TestNotifySocket_AgreesOnARelativeRoot(t *testing.T) {
	// A config carrying a relative root names the same library as the
	// absolute one the daemon resolves, so both must reach one socket.
	root := t.TempDir()
	t.Chdir(root)

	nested := filepath.Join(root, "library")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}

	if got, want := NotifySocket("library"), NotifySocket(nested); got != want {
		t.Errorf("NotifySocket(%q) = %q, want %q", "library", got, want)
	}
}

func TestSameRoot(t *testing.T) {
	root := t.TempDir()
	sep := string(filepath.Separator)
	other := filepath.Join(root, "other")

	// Built by concatenation rather than filepath.Join, which cleans its
	// arguments: a spelling normalized before the call would prove nothing
	// about the call.
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "the same path", a: root, b: root, want: true},
		{name: "a trailing separator", a: root, b: root + sep, want: true},
		{name: "an undotted path", a: root, b: root + sep + ".", want: true},
		{name: "a path through a parent", a: root, b: root + sep + "sub" + sep + "..", want: true},
		{name: "two different libraries", a: root, b: other, want: false},
		{name: "a prefix of another root", a: other, b: other + "-two", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameRoot(tt.a, tt.b); got != tt.want {
				t.Errorf("SameRoot(%q, %q) = %t, want %t", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestSameRoot_FoldsCaseOnlyWhereTheFilesystemDoes(t *testing.T) {
	// Two roots differing in case are one directory on Windows and two
	// everywhere else. Getting this backwards either refuses a repoint that
	// changes nothing, or silently accepts one that orphans a library.
	root := t.TempDir()

	want := runtime.GOOS == "windows"
	if got := SameRoot(root, strings.ToUpper(root)); got != want {
		t.Errorf("SameRoot(%q, upper case) = %t, want %t on %s", root, got, want, runtime.GOOS)
	}
}

func TestSameRoot_ResolvesARelativeRoot(t *testing.T) {
	// A config carrying a relative root names the same library as the
	// absolute path a command resolves, so comparing them must say so.
	root := t.TempDir()
	t.Chdir(root)

	nested := filepath.Join(root, "library")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}

	if !SameRoot("library", nested) {
		t.Errorf("SameRoot(%q, %q) = false, want the relative root resolved", "library", nested)
	}
}

func TestNotifySocket_StaysWithinTheAddressLimit(t *testing.T) {
	// A socket path over the limit does not bind, and the failure arrives at
	// the daemon's first event rather than at the operator.
	deep := filepath.Join(t.TempDir(), strings.Repeat("nested"+string(filepath.Separator), 30))
	t.Setenv(EnvDataDir, deep)

	socket := NotifySocket(t.TempDir())
	if len(socket) > MaxSocketPath {
		t.Errorf("NotifySocket = %q (%d bytes), want at most %d", socket, len(socket), MaxSocketPath)
	}
	if filepath.Dir(socket) != filepath.Clean(os.TempDir()) {
		t.Errorf("NotifySocket = %q, want it under the temporary directory %q", socket, os.TempDir())
	}
}

func TestNotifySocket_KeepsTheSocketExtension(t *testing.T) {
	// The name says what the file is, for anyone who finds one left behind.
	if got := filepath.Ext(NotifySocket(t.TempDir())); got != ".sock" {
		t.Errorf("NotifySocket extension = %q, want %q", got, ".sock")
	}
}

// ///////////////////////////////////////////////
// (Log).Path
// ///////////////////////////////////////////////

func TestLog_Path(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name       string
		log        Log
		wantSuffix string
	}{
		{
			name:       "registered daemon log",
			log:        LogDaemon,
			wantSuffix: "daemon.log",
		},
		{
			name:       "nested log",
			log:        Log{Name: "daemon", RelPath: filepath.Join("logs", "daemon.log")},
			wantSuffix: "daemon.log",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.log.Path(base)
			if !strings.HasPrefix(got, base) {
				t.Errorf("%s.Path(%q) = %q, want prefix %q", tt.name, base, got, base)
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("%s.Path(%q) = %q, want suffix %q", tt.name, base, got, tt.wantSuffix)
			}
		})
	}
}

func TestLog_Path_NestedContainsSubdir(t *testing.T) {
	base := t.TempDir()
	l := Log{Name: "daemon", RelPath: filepath.Join("logs", "daemon.log")}
	got := l.Path(base)
	if !strings.Contains(got, "logs") {
		t.Errorf("LogPath with nested RelPath = %q, want to contain %q", got, "logs")
	}
}

// ///////////////////////////////////////////////
// (Log).PathFor
// ///////////////////////////////////////////////

func TestLog_PathFor(t *testing.T) {
	l := Log{Name: "daemon", RelPath: "logs/daemon.log"}
	tests := []struct {
		name   string
		target TargetOS
		base   string
		want   string
	}{
		{name: "posix flat base", target: Posix, base: "/data/app", want: "/data/app/logs/daemon.log"},
		{name: "windows flat base", target: Windows, base: `C:\data\app`, want: `C:\data\app\logs\daemon.log`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := l.PathFor(tt.target, tt.base)
			if got != tt.want {
				t.Errorf("Log.PathFor(%v, %q) = %q, want %q", tt.target, tt.base, got, tt.want)
			}
		})
	}
}

func TestLog_PathFor_HostMatchesPath(t *testing.T) {
	base := t.TempDir()
	l := Log{Name: "daemon", RelPath: filepath.Join("logs", "daemon.log")}
	if got, want := l.PathFor(Host, base), l.Path(base); got != want {
		t.Errorf("Log.PathFor(Host) = %q, want same as Log.Path = %q", got, want)
	}
}

// ///////////////////////////////////////////////
// HomeDir
// ///////////////////////////////////////////////

func TestHomeDir(t *testing.T) {
	got := HomeDir()
	if got == "" {
		t.Error("HomeDir() returned empty string")
	}
}

func TestHomeDir_FallbackWhenUnset(t *testing.T) {
	// Clear every env var os.UserHomeDir consults so it falls through to
	// the error path, which HomeDir translates to ".".
	for _, key := range []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"} {
		t.Setenv(key, "")
	}
	if got := HomeDir(); got != "." {
		t.Errorf("HomeDir() with home env cleared = %q, want %q", got, ".")
	}
}

// ///////////////////////////////////////////////
// ConfigPath / ConfigPathFor
// ///////////////////////////////////////////////

func TestConfigPath(t *testing.T) {
	base := t.TempDir()
	got := ConfigPath(base)
	if !strings.HasPrefix(got, base) {
		t.Errorf("ConfigPath(%q) = %q, want prefix %q", base, got, base)
	}
	if !strings.HasSuffix(got, "config.toml") {
		t.Errorf("ConfigPath(%q) = %q, want suffix %q", base, got, "config.toml")
	}
}

func TestConfigPathFor(t *testing.T) {
	tests := []struct {
		name   string
		target TargetOS
		base   string
		want   string
	}{
		{name: "posix", target: Posix, base: "/data/app", want: "/data/app/config.toml"},
		{name: "windows", target: Windows, base: `C:\data\app`, want: `C:\data\app\config.toml`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConfigPathFor(tt.target, tt.base)
			if got != tt.want {
				t.Errorf("ConfigPathFor(%v, %q) = %q, want %q", tt.target, tt.base, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Library layout
// ///////////////////////////////////////////////

func TestStateDir_AnchorsTheLibraryLayout(t *testing.T) {
	// These are the on-disk layout of a library, and the layout is a
	// promise: a library created by one build is opened by the next. A
	// change to any of them relocates an existing library silently, so the
	// shape is pinned here rather than left to whoever edits the join.
	//
	// Everything machine-readable lives under the state directory, and the
	// two directories holding media sit beside it at the root where an
	// operator can see them.
	root := filepath.Join("libraries", "vods")
	state := filepath.Join(root, StateDirName)

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "state", got: StateDir(root), want: state},
		{name: "marker", got: MarkerPath(root), want: filepath.Join(state, "library.json")},
		{name: "database", got: DatabasePath(root), want: filepath.Join(state, "library.db")},
		{name: "trash", got: TrashDir(root), want: filepath.Join(state, "trash")},
		{name: "incoming", got: IncomingDir(root), want: filepath.Join(root, IncomingDirName)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("path = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestIncomingDir_SitsOutsideTheStateDirectory(t *testing.T) {
	// In-progress captures are the one thing an operator goes looking for by
	// hand, so they must not be buried in a dotted directory holding the
	// database. The two are separate for that reason and not by accident.
	root := filepath.Join("libraries", "vods")

	if incoming := IncomingDir(root); strings.HasPrefix(incoming, StateDir(root)) {
		t.Errorf("IncomingDir = %q, want it outside the state directory %q", incoming, StateDir(root))
	}
}

// ///////////////////////////////////////////////
// Program location
// ///////////////////////////////////////////////

func TestProgramPath_IsTheExecutableInsideTheProgramDirectory(t *testing.T) {
	// The installed path is what the scheduled task runs at every boot, so
	// it has to be an absolute address of a file rather than of a directory.
	// The directory itself is the platform's own per-user program location,
	// which is what makes owner-only write a property of the place.
	dir := ProgramDir()
	if dir == "" {
		t.Fatal("ProgramDir() = \"\", want the platform's per-user program directory")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("ProgramDir() = %q, want an absolute path", dir)
	}

	full := ProgramPath()
	if filepath.Dir(full) != dir {
		t.Errorf("ProgramPath() = %q, want it inside %q", full, dir)
	}
	if base := filepath.Base(full); base == "" || base == "." {
		t.Errorf("ProgramPath() = %q, want it to name the executable", full)
	}
	if runtime.GOOS == "windows" && filepath.Ext(full) != ".exe" {
		t.Errorf("ProgramPath() = %q, want the extension Windows runs", full)
	}
}

// ///////////////////////////////////////////////
// HomeRelative
// ///////////////////////////////////////////////

func TestHomeRelative(t *testing.T) {
	resolve := HomeRelative(".testapp")
	got := resolve()
	home := HomeDir()
	want := filepath.Join(home, ".testapp")
	if got != want {
		t.Errorf("HomeRelative(.testapp)() = %q, want %q", got, want)
	}
}

// ///////////////////////////////////////////////
// Fixed
// ///////////////////////////////////////////////

func TestFixed(t *testing.T) {
	tests := []struct {
		name string
		dir  string
	}{
		{name: "absolute unix", dir: "/data/myapp"},
		{name: "absolute windows", dir: `C:\data\myapp`},
		{name: "relative", dir: "relative/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolve := Fixed(tt.dir)
			if got := resolve(); got != tt.dir {
				t.Errorf("Fixed(%q)() = %q, want %q", tt.dir, got, tt.dir)
			}
		})
	}
}

// ///////////////////////////////////////////////
// EnvOr
// ///////////////////////////////////////////////

func TestEnvOr(t *testing.T) {
	const envKey = "TEST_PATHS_ENVVAR"
	fallback := Fixed("/fallback")

	t.Run("uses env when set", func(t *testing.T) {
		t.Setenv(envKey, "/from-env")
		resolve := EnvOr(envKey, fallback)
		if got := resolve(); got != "/from-env" {
			t.Errorf("EnvOr with env set = %q, want %q", got, "/from-env")
		}
	})

	t.Run("uses fallback when unset", func(t *testing.T) {
		resolve := EnvOr(envKey, fallback)
		if got := resolve(); got != "/fallback" {
			t.Errorf("EnvOr with env unset = %q, want %q", got, "/fallback")
		}
	})

	t.Run("uses fallback when empty", func(t *testing.T) {
		t.Setenv(envKey, "")
		resolve := EnvOr(envKey, fallback)
		if got := resolve(); got != "/fallback" {
			t.Errorf("EnvOr with env empty = %q, want %q", got, "/fallback")
		}
	})
}

// ///////////////////////////////////////////////
// Output paths
// ///////////////////////////////////////////////

func TestRequireAbsent(t *testing.T) {
	// The last moment this process can tell an occupied path from a free
	// one. Past it a capture reports an earlier recording's bytes as its
	// own, and a remux writes over a library file.
	dir := t.TempDir()

	file := filepath.Join(dir, "held.mkv")
	if err := os.WriteFile(file, []byte("an earlier recording"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", file, err)
	}
	subdir := filepath.Join(dir, "held")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", subdir, err)
	}

	tests := []struct {
		name    string
		what    string
		path    string
		wantErr bool
	}{
		{
			name: "a path nothing holds",
			what: "capture output",
			path: filepath.Join(dir, "free.mkv"),
		},
		{
			name:    "a file already there",
			what:    "capture output",
			path:    file,
			wantErr: true,
		},
		{
			// A directory holds the path as surely as a file does, and the
			// tool would fail on it in a way that names neither.
			name:    "a directory already there",
			what:    "output",
			path:    subdir,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequireAbsent(tt.what, tt.path)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("RequireAbsent() err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RequireAbsent() err = nil, want the occupied path refused")
			}
			if !strings.Contains(err.Error(), tt.what) {
				t.Errorf("RequireAbsent() err = %q, want it to name %q", err, tt.what)
			}
			if !strings.Contains(err.Error(), tt.path) {
				t.Errorf("RequireAbsent() err = %q, want it to name the path", err)
			}
		})
	}
}
