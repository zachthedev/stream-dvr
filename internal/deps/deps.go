// Package deps locates the external programs stream-dvr drives.
//
// Resolution happens at the point of use, never once at startup. A package
// manager can relocate a binary between an upgrade and the next recording,
// and a path captured hours earlier is a path that has already gone stale.
// Callers pay one PATH lookup per use, which is nothing beside the cost of
// spawning the process itself.
package deps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/procgroup"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Tool describes an external program and how to interrogate it.
type Tool struct {
	// Name is the executable's base name, without extension.
	Name string
	// Purpose explains what stream-dvr needs the tool for. It appears in
	// doctor output and in the error raised when the tool is missing.
	Purpose string
	// VersionArg is the flag that makes the tool print its version.
	VersionArg string
	// EnvOverride names an environment variable holding an explicit path.
	EnvOverride string
	// Hints match directory fragments in the platform fallback search.
	// A tool distributed inside a versioned directory needs one, because
	// its path changes with every upgrade.
	Hints []string
}

// Resolution is the outcome of locating one tool.
type Resolution struct {
	Tool    Tool
	Path    string
	Version string
	Source  Source
	Err     error
}

// Source records how a tool was found, so doctor can explain a surprise.
type Source string

// envPath is a search location expressed as an environment variable and the
// path below the directory it names.
type envPath struct {
	// env is the variable holding the base directory.
	env string
	// rest is the path below it, one element per component.
	rest []string
}

// Source values.
const (
	// SourceEnv means an environment override supplied the path.
	SourceEnv Source = "env"
	// SourcePath means the executable was found on PATH.
	SourcePath Source = "path"
	// SourceFallback means a platform-specific search found it.
	SourceFallback Source = "fallback"
	// SourceMissing means it was not found at all.
	SourceMissing Source = "missing"
)

// versionTimeout bounds a version probe. A tool that cannot answer this
// fast is broken, and a recording must not block on discovering that.
const versionTimeout = 10 * time.Second

// maxVersionOutput bounds how much of a version banner is read. The answer
// is one line, so this is far above any real one, and it is what stops a
// tool that writes without limit from sizing this process.
const maxVersionOutput = 64 << 10

// maxVersionText bounds the version itself. It is displayed in a fixed
// column beside three other tools, so a banner that is one long run without
// whitespace must not be able to take that column apart.
const maxVersionText = 128

// exeExtension is what Windows requires of a program's name. It is stated
// once: executableName appends it and isExecutable requires it, so a
// candidate is judged against exactly what resolution would have built.
const exeExtension = ".exe"

// peMagic opens every Windows executable image. Windows carries no execute
// bit, so this is what separates a program from a text file that was given
// a program's name.
const peMagic = "MZ"

// ErrNotFound reports a tool that could not be located.
var ErrNotFound = errors.New("executable not found")

// versionMarker finds the word a tool banner puts before its version.
//
// Case-insensitive against the original text, so the index it reports is
// valid for the string it is used to slice. Lowering a copy first and
// indexing the original is what turns a banner into a slice out of range.
var versionMarker = regexp.MustCompile(`(?i)version `)

// ///////////////////////////////////////////////
// Known tools
// ///////////////////////////////////////////////

// Streamlink captures live streams. It is the capture engine for every
// supported platform.
var Streamlink = Tool{
	Name:        "streamlink",
	Purpose:     "capture live streams",
	VersionArg:  "--version",
	EnvOverride: "STREAM_DVR_STREAMLINK",
}

// FFmpeg remuxes finished captures into their final container.
var FFmpeg = Tool{
	Name:        "ffmpeg",
	Purpose:     "remux captures and splice recovered gaps",
	VersionArg:  "-version",
	EnvOverride: "STREAM_DVR_FFMPEG",
	Hints:       []string{"ffmpeg"},
}

// FFprobe verifies a remux before the source file is discarded.
var FFprobe = Tool{
	Name:        "ffprobe",
	Purpose:     "verify remuxed output before discarding the source",
	VersionArg:  "-version",
	EnvOverride: "STREAM_DVR_FFPROBE",
	Hints:       []string{"ffmpeg"},
}

// YtDlp downloads past broadcasts during backfill.
var YtDlp = Tool{
	Name:        "yt-dlp",
	Purpose:     "download past broadcasts during backfill",
	VersionArg:  "--version",
	EnvOverride: "STREAM_DVR_YTDLP",
}

// All lists every external program stream-dvr can drive.
var All = []Tool{Streamlink, FFmpeg, FFprobe, YtDlp}

// packageRoots lists directories that hold one subdirectory per installed
// package. Both machine-scope and user-scope locations are searched,
// because a package manager can install to either.
//
// It is a variable so tests can point the search at a fixture tree on any
// platform, rather than only where a real package manager installs.
var packageRoots = defaultPackageRoots

// execCommand builds the version probe. It is a variable so tests can
// substitute a helper process and exercise what a tool prints without
// installing one.
var execCommand = exec.CommandContext

// ///////////////////////////////////////////////
// Resolution
// ///////////////////////////////////////////////

// Resolve locates a tool and probes its version.
//
// The returned Resolution always describes the attempt, including failures,
// so callers can report every tool's state in one pass. Check Err to decide
// whether Path is usable.
func Resolve(ctx context.Context, tool Tool) Resolution {
	path, source, err := Locate(tool)
	if err != nil {
		return Resolution{Tool: tool, Source: source, Err: err}
	}

	version, err := probeVersion(ctx, tool.Name, path, tool.VersionArg)
	return Resolution{Tool: tool, Path: path, Version: version, Source: source, Err: err}
}

// Locate finds a tool's executable and reports which strategy found it.
//
// Nothing here runs the tool. The checks stat and read, so a candidate that
// is not a program is refused before anything is spawned rather than after.
func Locate(tool Tool) (string, Source, error) {
	path, source, err := locate(tool)
	if err != nil {
		return "", source, err
	}
	if path == "" {
		return "", SourceMissing, fmt.Errorf("%s (%s): %w", tool.Name, tool.Purpose, ErrNotFound)
	}
	return path, source, nil
}

// ResolveAll locates every known tool.
func ResolveAll(ctx context.Context) []Resolution {
	out := make([]Resolution, 0, len(All))
	for _, tool := range All {
		out = append(out, Resolve(ctx, tool))
	}
	return out
}

// Path returns a usable path to the tool, or an error describing why it is
// unavailable. Use it at the call site that is about to spawn the process.
//
// It takes no context because it starts nothing, and that is the point. A
// version probe is a subprocess, and running one on the way to every
// capture spends its cost per channel per poll and lets a tool that is slow
// to print a banner abandon the recording it was clearing the way for. The
// version belongs to doctor, which asks for it through Resolve.
func Path(tool Tool) (string, error) {
	path, _, err := Locate(tool)
	return path, err
}

// locate finds a tool's executable, reporting which strategy succeeded.
//
// A relative result is refused whatever produced it. Such a path is
// resolved against the working directory when the process is spawned, and
// that directory belongs to whoever started the daemon.
func locate(tool Tool) (string, Source, error) {
	path, source, err := search(tool)
	if err != nil || path == "" {
		return "", source, err
	}

	if !filepath.IsAbs(path) {
		if source == SourceEnv {
			return "", source, fmt.Errorf("%s names %s, which is not an absolute path: %w",
				tool.EnvOverride, path, ErrNotFound)
		}
		return "", SourceMissing, nil
	}
	return path, source, nil
}

// search tries each resolution strategy in precedence order.
func search(tool Tool) (string, Source, error) {
	if tool.EnvOverride != "" {
		if custom := os.Getenv(tool.EnvOverride); custom != "" {
			// An override is set to pin one copy of a tool, which means the
			// search it replaces is a search the operator ruled out. Falling
			// back to it would run a different binary than the one named.
			if !isExecutable(custom) {
				return "", SourceEnv, fmt.Errorf("%s names %s, which is not an executable file: %w",
					tool.EnvOverride, custom, ErrNotFound)
			}
			return custom, SourceEnv, nil
		}
	}
	// isExecutable here too, as on every other branch. LookPath honours
	// PATHEXT, which puts .COM ahead of .EXE, so without it a script beside
	// a real tool is what gets spawned.
	if found, err := exec.LookPath(tool.Name); err == nil && isExecutable(found) {
		return found, SourcePath, nil
	}
	if found := searchFallbacks(tool); found != "" {
		return found, SourceFallback, nil
	}
	return "", SourceMissing, nil
}

// searchFallbacks looks in the well-known install locations for the host
// platform, trying fixed layouts before searching package directories.
func searchFallbacks(tool Tool) string {
	for _, pattern := range exactPatterns(tool) {
		if found := firstMatch(pattern); found != "" {
			return found
		}
	}
	return searchPackageRoots(tool)
}

// exactPatterns returns paths whose layout does not change between
// releases of the tool.
func exactPatterns(tool Tool) []string {
	if runtime.GOOS != "windows" {
		return unixPatterns(tool)
	}

	exe := executableName(tool.Name)
	return underEnv(
		envPath{env: "LOCALAPPDATA", rest: []string{"Microsoft", "WinGet", "Links", exe}},
		envPath{env: "ProgramFiles", rest: []string{"WinGet", "Links", exe}},
		envPath{env: "LOCALAPPDATA", rest: []string{"Programs", tool.Name, "bin", exe}},
	)
}

// unixPatterns returns the fixed install locations on macOS and Linux.
func unixPatterns(tool Tool) []string {
	dirs := []string{
		// Homebrew on Apple silicon leads /usr/local/bin, which is where a
		// Rosetta x86_64 build lands. Taking the translated one would run
		// every transcode through emulation on a machine that has a native
		// build installed.
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/opt/local/bin",
		"/snap/bin",
		"/var/lib/flatpak/exports/bin",
		"/home/linuxbrew/.linuxbrew/bin",
	}

	// pipx puts both streamlink and yt-dlp here, which is how a Python tool
	// arrives on macOS and Linux. These come last so a per-user install
	// does not shadow a machine-wide one.
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".nix-profile", "bin"))
	}

	patterns := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		patterns = append(patterns, filepath.Join(dir, tool.Name))
	}
	return patterns
}

// defaultPackageRoots returns the host platform's package directories,
// machine scope first.
//
// Order decides which install wins, so the root the account cannot write
// to is searched before the one it can.
func defaultPackageRoots() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	return underEnv(
		envPath{env: "ProgramFiles", rest: []string{"WinGet", "Packages"}},
		envPath{env: "LOCALAPPDATA", rest: []string{"Microsoft", "WinGet", "Packages"}},
	)
}

// underEnv joins each path onto the directory its environment variable
// names, dropping any whose variable is unset.
//
// An empty variable makes Join drop the element and return a relative path
// such as "Microsoft\WinGet\Links\ffmpeg.exe", which then resolves against
// the working directory. The check has to happen before the Join, because
// the Join is where the distinction is lost.
func underEnv(candidates ...envPath) []string {
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		root := os.Getenv(candidate.env)
		if root == "" {
			continue
		}
		paths = append(paths, filepath.Join(append([]string{root}, candidate.rest...)...))
	}
	return paths
}

// searchPackageRoots finds a tool nested inside a package manager's
// per-package directories.
//
// Two properties force a directory scan rather than a glob. The package
// directory is named after the publisher, whose capitalization differs from
// the tool's own name, and filepath.Glob matches case-sensitively even on
// Windows. The build directory nested inside carries a version string that
// changes on every upgrade. Matching is therefore case-insensitive on a
// hint, with a wildcard for the build directory.
//
// Roots are tried in order and the first one holding a match wins. Ranking
// every root together by modification time lets a package directory this
// account can write to beat a machine-scope install simply by being
// touched. Within one root the newest still wins, which is the upgrade case
// this search exists for.
func searchPackageRoots(tool Tool) string {
	if len(tool.Hints) == 0 {
		return ""
	}

	exe := executableName(tool.Name)
	for _, root := range packageRoots() {
		if found := newestInRoot(root, tool.Hints, exe); found != "" {
			return found
		}
	}
	return ""
}

// newestInRoot returns the most recently modified executable under one
// package root.
func newestInRoot(root string, hints []string, exe string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}

	var (
		newest     string
		newestTime time.Time
	)
	for _, entry := range entries {
		if !entry.IsDir() || !matchesHint(entry.Name(), hints) {
			continue
		}
		pkg := filepath.Join(root, entry.Name())
		for _, pattern := range []string{
			filepath.Join(pkg, "bin", exe),
			filepath.Join(pkg, "*", "bin", exe),
			filepath.Join(pkg, exe),
		} {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				continue
			}
			for _, match := range matches {
				info, err := os.Stat(match)
				if err != nil || !isExecutable(match) {
					continue
				}
				if info.ModTime().After(newestTime) {
					newest, newestTime = match, info.ModTime()
				}
			}
		}
	}
	return newest
}

// matchesHint reports whether a package directory holds the named tool.
//
// A package directory is "<Publisher>.<Package>_<source>", so the package
// is the text after the first dot and before the first underscore, and that
// segment alone is compared. Searching the whole name for the word instead
// accepts any directory that merely mentions it: this root grants the
// account FullControl, so "totally.legit.ffmpeg.helper" would decide which
// ffmpeg remuxes every recording.
func matchesHint(name string, hints []string) bool {
	segment := packageSegment(name)
	for _, hint := range hints {
		if strings.EqualFold(segment, hint) {
			return true
		}
	}
	return false
}

// packageSegment returns the package's own name from a package directory.
func packageSegment(name string) string {
	if _, afterPublisher, ok := strings.Cut(name, "."); ok {
		name = afterPublisher
	}
	packageName, _, _ := strings.Cut(name, "_")
	return packageName
}

// executableName appends the platform's executable extension.
func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + exeExtension
	}
	return name
}

// firstMatch returns the first existing regular file matching pattern.
func firstMatch(pattern string) string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}
	for _, match := range matches {
		if isExecutable(match) {
			return match
		}
	}
	return ""
}

// isExecutable reports whether path names a program this process could run.
//
// Taking an arbitrary path is the point: resolution walks PATH and the
// package directories to find out where a tool landed, and a candidate it
// is not allowed to stat is a candidate it cannot check.
//
// Nothing here runs the file. A regular file is not enough on either
// platform, because an environment override or a planted package directory
// can name a text file, and accepting one turns resolution into a spawn
// that fails at the moment a broadcast starts.
//
//nolint:gosec // G703: locating a tool is what this does
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS != "windows" {
		return info.Mode()&0o111 != 0
	}

	// Windows mode bits carry no execute permission, so the name and the
	// file's own header answer instead. The extension alone is a rename
	// away from anything.
	if !strings.EqualFold(filepath.Ext(path), exeExtension) {
		return false
	}
	return hasImageHeader(path)
}

// hasImageHeader reports whether a file opens with an executable image's
// signature.
//
//nolint:gosec // G304: reading a candidate's first bytes is how this decides it is a program
func hasImageHeader(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	header := make([]byte, len(peMagic))
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return string(header) == peMagic
}

// probeVersion runs the tool's version flag and returns the version it
// reports, reduced to the version string itself.
func probeVersion(ctx context.Context, name, path, versionArg string) (string, error) {
	if versionArg == "" {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	stdout := procgroup.NewOutput(maxVersionOutput)
	cmd := execCommand(ctx, path, versionArg)
	cmd.Stdout = stdout
	if err := procgroup.Run(cmd); err != nil {
		return "", fmt.Errorf("probing %s version: %w", path, err)
	}
	// A prefix is a usable answer here, unlike one taken from a body that is
	// parsed. The version is the first line and nothing past it is read, so
	// a tool that keeps writing has already said what it was asked.
	line, _, _ := strings.Cut(strings.TrimSpace(stdout.String()), "\n")
	return normalizeVersion(name, line), nil
}

// normalizeVersion reduces a tool's version banner to the version itself.
//
// The three shapes in use are "ffmpeg version 9.0-x Copyright (c) ...",
// "streamlink 8.4.0", and a bare "2026.07.04". Callers display this in a
// fixed column, so a full banner would crowd out everything beside it.
//
// The result is escaped here rather than at each display, because it is a
// string a program on PATH chose and it reaches a terminal and a log.
func normalizeVersion(name, line string) string {
	text := strings.TrimSpace(line)
	if text == "" {
		return ""
	}

	// Searched in the original rather than in a lowered copy. Lowering is
	// not length-preserving: U+023A is two bytes and lowers to a three-byte
	// rune, so an index taken from the lowered text can point past the end
	// of the text it is used to slice. Two such runes ahead of the marker
	// panic this function, and one makes it cut in the wrong place and
	// report a version the tool never stated. Every byte here came from a
	// program on PATH printing its banner.
	if loc := versionMarker.FindStringIndex(text); loc != nil {
		text = text[loc[1]:]
	}

	fields := strings.Fields(text)
	switch {
	case len(fields) == 0:
		return ""
	case len(fields) > 1 && strings.EqualFold(fields[0], name):
		return escape.Text(clip(fields[1]))
	default:
		return escape.Text(clip(fields[0]))
	}
}

// clip bounds a version to what a table cell can hold.
//
// The banner is a prefix of whatever the tool wrote, and a tool that writes
// one long run without whitespace yields a single field of that whole run.
// It is cut before escaping, so the cut lands on the tool's own text rather
// than inside a sequence this package rendered.
func clip(version string) string {
	if len(version) <= maxVersionText {
		return version
	}

	cut := maxVersionText
	for cut > 0 && !utf8.RuneStart(version[cut]) {
		cut--
	}
	return version[:cut]
}
