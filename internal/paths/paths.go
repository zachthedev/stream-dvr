// Package paths provides centralized path accessors for stream-dvr, and the
// one check a path is put through before an external tool writes it.
//
// Three roots exist and must not be confused. The data dir holds
// machine-local config and credentials. The log dir holds machine-local
// logs, which two of the three platforms keep somewhere other than the
// config. The library root holds recordings and is chosen by the operator,
// often on a separate volume, so every path inside it, including that
// library's own logs, is a function of a root passed in by the caller rather
// than a global.
//
// It is a leaf package with no module-internal imports, safe to import from
// anywhere. Keep it that way, so nothing here can pull a cycle behind it.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adrg/xdg"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Log describes a registered log file.
type Log struct {
	Name    string // identifier: "daemon", "tui"
	RelPath string // relative to base dir: "logs/daemon.log"
}

// TargetOS selects the path-separator style used when joining paths.
// Use it when generating paths destined for a platform other than the
// host (e.g., a config file generated on a Windows dev box that ships
// inside a Linux binary).
type TargetOS int

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// BinaryName is the canonical name of the project binary.
const BinaryName = "stream-dvr"

// EnvDataDir overrides the machine-local data directory. The log directory
// and the runtime directory follow it, so one variable moves everything
// machine-local.
const EnvDataDir = "STREAM_DVR_HOME"

// envStateHome is where the XDG base directory specification puts state that
// is neither config nor cache, which is the category logs fall in.
const envStateHome = "XDG_STATE_HOME"

// envRuntimeDir is where the XDG base directory specification puts files
// that exist only while a program is running.
const envRuntimeDir = "XDG_RUNTIME_DIR"

// envLocalAppData is the Windows per-user directory that does not roam.
const envLocalAppData = "LocalAppData"

// MaxSocketPath is the longest path an AF_UNIX socket can bind.
//
// The address structure carries the path in a 108-byte field including its
// terminator, so 107 bytes is the limit. Windows enforces the same one:
// measured there, 107 binds and 108 fails with "invalid argument".
//
// Whoever binds a socket checks this, because the operating system's own
// refusal is "invalid argument" and names nothing.
const MaxSocketPath = 107

// StateDirName is the library subdirectory holding all machine-readable
// state: the database, the ownership marker, and per-library logs. It is
// dot-prefixed so media scanners skip it.
const StateDirName = ".dvr"

// IncomingDirName is the library subdirectory holding in-progress captures.
const IncomingDirName = "incoming"

// DataDirMode keeps the data directory to its owner.
//
// It holds a live OAuth token, the credential file, and a config carrying a
// webhook URL that is itself a credential, so nothing but the owner reaches
// it. os.MkdirAll does nothing to a directory that is already there, so
// whichever command an operator runs first is the one that decides the mode.
const DataDirMode os.FileMode = 0o700

// TargetOS values.
const (
	// Host uses the host operating system's native separator.
	// Equivalent to filepath.Join.
	Host TargetOS = iota
	// Posix always uses forward slashes. Suitable for Linux, macOS,
	// and other Unix-like targets.
	Posix
	// Windows always uses backslashes.
	Windows
)

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

// ReservedDirNames are the directories a library keeps for itself. A
// rendered name must never resolve to one, or recordings land in the state
// directory or the capture directory rather than the library.
var ReservedDirNames = []string{StateDirName, IncomingDirName}

// LogDaemon receives the recording daemon's output.
var LogDaemon = Log{Name: "daemon", RelPath: "logs/daemon.log"}

// LogTUI receives the interactive client's output. The TUI owns the
// terminal, so its diagnostics cannot go to stderr.
var LogTUI = Log{Name: "tui", RelPath: "logs/tui.log"}

// LogCapture receives the capture subprocesses' own diagnostics, kept apart
// from the daemon's log because the two are written differently.
//
// The daemon rotates by renaming the file once it passes a size limit. A
// capture child holds its log open for the length of a broadcast, which on
// Windows blocks that rename. A rotation that cannot rename discards the
// records it was moving. Several hours-long children aimed at the file the
// daemon is rotating lose exactly the lines explaining what went wrong.
var LogCapture = Log{Name: "capture", RelPath: "logs/capture.log"}

// LogNotifyAgent receives the notify agent's output. The agent has no
// terminal at all: the session starts it, and its console is hidden at the
// first opportunity.
var LogNotifyAgent = Log{Name: "notify-agent", RelPath: "logs/notify-agent.log"}

// Logs lists all log files the project writes.
var Logs = []Log{LogDaemon, LogTUI, LogCapture, LogNotifyAgent}

// DataDir returns the machine-local configuration directory. It holds the
// config file and credentials, never recordings and never logs.
var DataDir = EnvOr(EnvDataDir, configRelative(BinaryName))

// LogDir returns the machine-local log directory, for logs that belong to
// the installation rather than to one library.
//
// It is separate from DataDir because only Windows keeps both in one place.
// A library's own logs are not here: they sit in that library's state
// directory, so a library carries its history when it moves.
var LogDir = EnvOr(EnvDataDir, func() string {
	return logRoot(runtime.GOOS, HomeDir(), os.Getenv(envStateHome), DataDir())
})

// RuntimeDir returns the directory for files that exist only while
// something is running, which here means the notification socket.
//
// It is not the library root. A library often sits on a separate volume and
// is sometimes a network share, where a socket cannot bind at all. It is
// not the roaming application data directory either: a path that followed
// the operator to another machine would name a socket nothing is listening
// on.
var RuntimeDir = EnvOr(EnvDataDir, func() string {
	return runtimeRoot(runtime.GOOS, os.Getenv(envRuntimeDir), os.Getenv(envLocalAppData), DataDir())
})

// ///////////////////////////////////////////////
// Path accessors
// ///////////////////////////////////////////////

// Path returns the full path to this log file within a base directory,
// using the host operating system's native separator.
func (l Log) Path(baseDir string) string {
	return filepath.Join(baseDir, l.RelPath)
}

// PathFor returns the full path to this log file within a base directory,
// using the separator style selected by target. Use it when generating
// paths consumed on a platform other than the host.
func (l Log) PathFor(target TargetOS, baseDir string) string {
	return joinFor(target, baseDir, l.RelPath)
}

// HomeDir returns the user's home directory, falling back to "." on error.
func HomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

// ProgramDir names where an installed copy of the binary belongs.
//
// An autostart entry runs whatever stands at the path it recorded, so that
// path decides what runs as the operator at every boot. xdg.BinHome resolves
// to the per-user program directory each platform uses. All of them sit under
// the profile, where only the owner may write, which makes the restriction a
// property of the place rather than a check somebody has to remember. A
// binary left where it was downloaded carries whatever its directory allows,
// and a folder made at the root of a Windows volume inherits write access
// for every authenticated user.
func ProgramDir() string {
	return xdg.BinHome
}

// ProgramPath returns the installed binary's full path.
func ProgramPath() string {
	return filepath.Join(ProgramDir(), executableName(runtime.GOOS))
}

// ConfigPath returns the path to the config file within a base directory,
// using the host operating system's native separator.
func ConfigPath(baseDir string) string {
	return filepath.Join(baseDir, "config.toml")
}

// ConfigPathFor returns the path to the config file within a base directory,
// using the separator style selected by target.
func ConfigPathFor(target TargetOS, baseDir string) string {
	return joinFor(target, baseDir, "config.toml")
}

// ///////////////////////////////////////////////
// Library accessors
// ///////////////////////////////////////////////

// StateDir returns the state directory inside a library root.
func StateDir(libraryRoot string) string {
	return filepath.Join(libraryRoot, StateDirName)
}

// MarkerPath returns the ownership marker inside a library root. The marker
// records which build owns the library and gates every open.
func MarkerPath(libraryRoot string) string {
	return filepath.Join(StateDir(libraryRoot), "library.json")
}

// DatabasePath returns the SQLite database inside a library root. It sits
// beside the ownership marker and is named for what it describes, since the
// enclosing directory already says whose it is.
func DatabasePath(libraryRoot string) string {
	return filepath.Join(StateDir(libraryRoot), "library.db")
}

// IncomingDir returns the directory holding in-progress captures. Files
// live here under metadata-free names until the organizer can render a
// validated final name.
func IncomingDir(libraryRoot string) string {
	return filepath.Join(libraryRoot, IncomingDirName)
}

// TrashDir returns the directory holding deleted recordings during their
// grace period.
func TrashDir(libraryRoot string) string {
	return filepath.Join(StateDir(libraryRoot), "trash")
}

// SameRoot reports whether two library roots name one directory.
//
// A trailing separator, a relative path, and on Windows a difference of
// case all name the same library, so a caller comparing what an operator
// typed against what a config already holds compares the canonical forms.
func SameRoot(a, b string) bool {
	return canonicalRoot(a) == canonicalRoot(b)
}

// NotifySocket returns the notification socket carrying one library's
// events.
//
// The name carries a digest of the library root so that two libraries
// recorded on one machine do not land on one socket. The digest is not a
// secret: it names a path, and whatever can open the socket can already
// read every event on it.
//
// The path is bounded, and a runtime directory deep enough to overrun the
// address field falls back to the temporary directory. A socket somewhere
// less tidy beats one that cannot bind.
func NotifySocket(libraryRoot string) string {
	sum := sha256.Sum256([]byte(canonicalRoot(libraryRoot)))
	name := "notify-" + hex.EncodeToString(sum[:4]) + ".sock"

	socket := filepath.Join(RuntimeDir(), name)
	if len(socket) <= MaxSocketPath {
		return socket
	}
	return filepath.Join(os.TempDir(), name)
}

// canonicalRoot reduces a library root to one form, so that the daemon and
// the agent derive one socket from what an operator wrote two ways.
//
// A trailing separator, a relative path, and on Windows a difference of
// case all name the same directory. Only Windows folds case, because
// elsewhere two roots differing in case are two directories.
func canonicalRoot(libraryRoot string) string {
	canonical, err := filepath.Abs(libraryRoot)
	if err != nil {
		// An unresolvable root still has to hash to something stable, and
		// the cleaned form is the best available answer.
		canonical = filepath.Clean(libraryRoot)
	}
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	return canonical
}

// joinFor joins parts using the separator style selected by target. Posix
// uses forward slashes, Windows uses backslashes, and Host delegates to
// filepath.Join.
func joinFor(target TargetOS, parts ...string) string {
	switch target {
	case Posix:
		return path.Join(parts...)
	case Windows:
		return strings.ReplaceAll(path.Join(parts...), "/", `\`)
	default:
		return filepath.Join(parts...)
	}
}

// ///////////////////////////////////////////////
// Resolver helpers
// ///////////////////////////////////////////////

// HomeRelative returns a resolver that joins HomeDir() with rel.
//
//	var DataDir = HomeRelative(".myapp") // ~/.myapp
func HomeRelative(rel string) func() string {
	return func() string {
		return filepath.Join(HomeDir(), rel)
	}
}

// Fixed returns a resolver that always returns dir.
//
//	var DataDir = Fixed("/data/myapp")
func Fixed(dir string) func() string {
	return func() string { return dir }
}

// EnvOr returns a resolver that reads envKey, falling back to fallback().
//
//	var DataDir = EnvOr("MYAPP_DATA", HomeRelative(".myapp"))
func EnvOr(envKey string, fallback func() string) func() string {
	return func() string {
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		return fallback()
	}
}

// configRelative returns a resolver rooted at the OS config directory, which
// is %AppData% on Windows, ~/Library/Application Support on macOS, and
// $XDG_CONFIG_HOME or ~/.config elsewhere. It falls back to a dotted home
// directory when the OS location is unavailable.
func configRelative(rel string) func() string {
	return func() string {
		dir, err := os.UserConfigDir()
		if err != nil {
			return filepath.Join(HomeDir(), "."+rel)
		}
		return filepath.Join(dir, rel)
	}
}

// logRoot returns the directory a platform keeps application logs under.
//
// Only Windows has one per-application location, so its logs sit with the
// config in dataDir. macOS has ~/Library/Logs, which is where Console looks.
// Everywhere else the XDG base directory specification applies, and it puts
// logs under XDG_STATE_HOME rather than beside the config: state that
// survives a restart and that a user would not hand-edit.
func logRoot(goos, home, stateHome, dataDir string) string {
	switch goos {
	case "windows":
		return dataDir
	case "darwin":
		return filepath.Join(home, "Library", "Logs", BinaryName)
	default:
		if stateHome == "" {
			stateHome = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(stateHome, BinaryName)
	}
}

// runtimeRoot returns the directory a platform keeps running-process files
// under.
//
// Linux has XDG_RUNTIME_DIR, a per-user directory the session manager
// creates and empties at logout, which is exactly a socket's lifetime.
// Windows has the local application data directory, which is per-user and
// does not roam. macOS has neither, so the data directory serves, and a
// stale socket there is removed at the next bind.
func runtimeRoot(goos, runtimeHome, localAppData, dataDir string) string {
	switch goos {
	case "windows":
		if localAppData == "" {
			return dataDir
		}
		return filepath.Join(localAppData, BinaryName)
	case "darwin":
		return dataDir
	default:
		if runtimeHome == "" {
			return dataDir
		}
		return filepath.Join(runtimeHome, BinaryName)
	}
}

// executableName returns the binary's file name on a platform.
//
// The suffix is hand-written because the standard library exposes no helper
// for it and every alternative is a build-tagged file for one string.
func executableName(goos string) string {
	if goos == "windows" {
		return BinaryName + ".exe"
	}
	return BinaryName
}

// ///////////////////////////////////////////////
// Output paths
// ///////////////////////////////////////////////

// RequireAbsent refuses a path something already holds, naming it with
// what.
//
// It runs before a path is handed to an external tool that will write it,
// which is the last moment this process can tell an occupied path from a
// free one. Once the tool has run, the file there answers for the tool and
// for whatever was already at that path.
func RequireAbsent(what, path string) error {
	switch _, err := os.Stat(path); {
	case err == nil:
		return fmt.Errorf("%s %s already exists", what, path)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("checking %s %s: %w", what, path, err)
	}
}
