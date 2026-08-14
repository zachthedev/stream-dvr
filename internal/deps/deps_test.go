package deps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helper process
// ///////////////////////////////////////////////

// TestHelperProcess stands in for a tool answering its version flag. It is
// not a test: it runs only when the parent re-invokes this binary.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("helper process, invoked only by fakeExec")
	}

	switch os.Getenv("FAKE_MODE") {
	case "banner":
		os.Stdout.WriteString("ffmpeg version 9.0 Copyright (c) 2000-2026 the FFmpeg developers\n")

	case "flood":
		// A tool that answers a one-line question without limit. The banner
		// itself is intact, which is the whole answer this reads.
		os.Stdout.WriteString("ffmpeg version 9.0 Copyright (c) 2000-2026 the FFmpeg developers\n")
		for range 300 {
			os.Stdout.WriteString(strings.Repeat("padding ", 1024))
		}

	case "flood_one_run":
		// One long run with no whitespace anywhere, so the banner never ends
		// and the version is whatever was kept of it.
		os.Stdout.WriteString("ffmpeg version ")
		for range 300 {
			os.Stdout.WriteString(strings.Repeat("9", 1024))
		}
	}
	os.Exit(0)
}

// fakeExec redirects version probes to the helper process.
func fakeExec(t *testing.T, mode string) {
	t.Helper()

	// The helper is this test binary, so under -cover it carries the same
	// instrumentation. Given nowhere to write its profile, the runtime
	// prints a warning on the helper's own streams, which is where this
	// package reads a version from.
	coverDir := t.TempDir()

	original := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		helper := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helper...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"FAKE_MODE="+mode,
			"GOCOVERDIR="+coverDir,
		)
		return cmd
	}
	t.Cleanup(func() { execCommand = original })
}

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// writeFakeTool creates a file at dir/name that resolution will accept as
// an executable: the exec bit exec.LookPath requires on Unix, and the
// extension and image header required on Windows.
//
// It is deliberately not a runnable program. Every test here is about
// which file is chosen, and a probe that tried to run this one fails, which
// is the state a version probe is meant to survive.
func writeFakeTool(t *testing.T, dir, name string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	path := filepath.Join(dir, executableName(name))
	if err := os.WriteFile(path, []byte(peMagic+"-fake"), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// writeTextFile creates a file that is not a program, under whatever name
// it is given.
func writeTextFile(t *testing.T, dir, name string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("this is not a program"), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// setModTime pins a file's modification time so "newest wins" is decidable
// rather than dependent on how fast the test filesystem is.
func setModTime(t *testing.T, path string, when time.Time) {
	t.Helper()

	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("setting mtime on %s: %v", path, err)
	}
}

// usePackageRoots points the package-directory search at fixture roots for
// the duration of one test.
func usePackageRoots(t *testing.T, roots ...string) {
	t.Helper()

	original := packageRoots
	packageRoots = func() []string { return roots }
	t.Cleanup(func() { packageRoots = original })
}

// ///////////////////////////////////////////////
// normalizeVersion
// ///////////////////////////////////////////////

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		name string
		tool string
		line string
		want string
	}{
		{
			name: "ffmpeg banner with copyright",
			tool: "ffmpeg",
			line: "ffmpeg version 9.0-full_build-www.gyan.dev Copyright (c) 2000-2026 the FFmpeg developers",
			want: "9.0-full_build-www.gyan.dev",
		},
		{
			name: "ffprobe banner with copyright",
			tool: "ffprobe",
			line: "ffprobe version 7.1 Copyright (c) 2007-2026 the FFmpeg developers",
			want: "7.1",
		},
		{
			name: "name and version only",
			tool: "streamlink",
			line: "streamlink 8.4.0",
			want: "8.4.0",
		},
		{
			name: "bare version",
			tool: "yt-dlp",
			line: "2026.07.04",
			want: "2026.07.04",
		},
		{
			name: "version keyword in mixed case",
			tool: "tool",
			line: "Tool Version 1.2.3",
			want: "1.2.3",
		},
		{
			name: "surrounding whitespace",
			tool: "yt-dlp",
			line: "   2026.07.04   ",
			want: "2026.07.04",
		},
		{
			name: "empty line",
			tool: "ffmpeg",
			line: "",
			want: "",
		},
		{
			name: "whitespace only",
			tool: "ffmpeg",
			line: "   ",
			want: "",
		},
		{
			name: "name only with no version",
			tool: "streamlink",
			line: "streamlink",
			want: "streamlink",
		},
		{
			name: "name case differs from banner",
			tool: "yt-dlp",
			line: "YT-DLP 2026.07.04",
			want: "2026.07.04",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeVersion(tt.tool, tt.line); got != tt.want {
				t.Errorf("normalizeVersion(%q, %q) = %q, want %q", tt.tool, tt.line, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// matchesHint
// ///////////////////////////////////////////////

func TestMatchesHint(t *testing.T) {
	tests := []struct {
		name  string
		dir   string
		hints []string
		want  bool
	}{
		{
			name:  "publisher prefix with differing case",
			dir:   "Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe",
			hints: []string{"ffmpeg"},
			want:  true,
		},
		{
			name:  "second publisher for the same tool",
			dir:   "yt-dlp.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe",
			hints: []string{"ffmpeg"},
			want:  true,
		},
		{
			name:  "hint in upper case matches lower directory",
			dir:   "gyan.ffmpeg_source",
			hints: []string{"FFMPEG"},
			want:  true,
		},
		{
			name:  "unrelated package",
			dir:   "Microsoft.PowerShell_8wekyb3d8bbwe",
			hints: []string{"ffmpeg"},
			want:  false,
		},
		{
			name:  "second hint matches",
			dir:   "SomeVendor.AVConv",
			hints: []string{"ffmpeg", "avconv"},
			want:  true,
		},
		{
			name:  "no hints never matches",
			dir:   "Gyan.FFmpeg_Source",
			hints: nil,
			want:  false,
		},
		{
			// This root grants the account FullControl, so a directory
			// planted here that merely contains the word would decide which
			// ffmpeg remuxes every recording.
			name:  "a planted directory naming the tool in its own package part",
			dir:   "totally.legit.ffmpeg.helper",
			hints: []string{"ffmpeg"},
			want:  false,
		},
		{
			name:  "a package whose name only begins with the hint",
			dir:   "Vendor.FFmpegTools_Source",
			hints: []string{"ffmpeg"},
			want:  false,
		},
		{
			name:  "a package with no publisher prefix",
			dir:   "ffmpeg",
			hints: []string{"ffmpeg"},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesHint(tt.dir, tt.hints); got != tt.want {
				t.Errorf("matchesHint(%q, %v) = %t, want %t", tt.dir, tt.hints, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// searchPackageRoots
// ///////////////////////////////////////////////

func TestSearchPackageRoots_NewestBuildWins(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe")

	older := writeFakeTool(t, filepath.Join(pkg, "ffmpeg-9.0-full_build", "bin"), "ffmpeg")
	newer := writeFakeTool(t, filepath.Join(pkg, "ffmpeg-9.1-full_build", "bin"), "ffmpeg")
	setModTime(t, older, time.Now().Add(-48*time.Hour))
	setModTime(t, newer, time.Now())

	usePackageRoots(t, root)

	if got := searchPackageRoots(FFmpeg); got != newer {
		t.Errorf("searchPackageRoots(FFmpeg) = %q, want the newer build %q", got, newer)
	}
}

func TestSearchPackageRoots_TheFirstRootHoldingAMatchWins(t *testing.T) {
	// Roots are listed machine scope first. Ranking every root together by
	// modification time lets a package directory this account can write to
	// beat a machine-scope install simply by being touched, and touching a
	// file is not a privilege anything can withhold.
	machine := t.TempDir()
	user := t.TempDir()

	want := writeFakeTool(t, filepath.Join(machine, "Gyan.FFmpeg_Source", "bin"), "ffmpeg")
	newer := writeFakeTool(t, filepath.Join(user, "Gyan.FFmpeg_Source", "bin"), "ffmpeg")
	setModTime(t, want, time.Now().Add(-48*time.Hour))
	setModTime(t, newer, time.Now())

	usePackageRoots(t, machine, user)

	if got := searchPackageRoots(FFmpeg); got != want {
		t.Errorf("searchPackageRoots(FFmpeg) = %q, want the machine-scope copy %q", got, want)
	}
}

func TestSearchPackageRoots_FallsToTheNextRootWhenTheFirstHasNothing(t *testing.T) {
	empty := t.TempDir()
	user := t.TempDir()
	want := writeFakeTool(t, filepath.Join(user, "Gyan.FFmpeg_Source", "bin"), "ffmpeg")

	usePackageRoots(t, empty, user)

	if got := searchPackageRoots(FFmpeg); got != want {
		t.Errorf("searchPackageRoots(FFmpeg) = %q, want %q", got, want)
	}
}

func TestSearchPackageRoots_RefusesAPlantedPackageDirectory(t *testing.T) {
	// A directory planted under this root must not become the ffmpeg every
	// remux runs through. One named this way won a live scan.
	root := t.TempDir()
	writeFakeTool(t, filepath.Join(root, "totally.legit.ffmpeg.helper", "bin"), "ffmpeg")

	usePackageRoots(t, root)

	if got := searchPackageRoots(FFmpeg); got != "" {
		t.Errorf("searchPackageRoots(FFmpeg) = %q, want a planted package ignored", got)
	}
}

func TestSearchPackageRoots_RefusesAFileThatIsNotAProgram(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the fixture carries the exec bit, which is all Unix asks for")
	}

	root := t.TempDir()
	writeTextFile(t, filepath.Join(root, "Gyan.FFmpeg_Source", "bin"), "ffmpeg"+exeExtension)

	usePackageRoots(t, root)

	if got := searchPackageRoots(FFmpeg); got != "" {
		t.Errorf("searchPackageRoots(FFmpeg) = %q, want a text file refused", got)
	}
}

func TestSearchPackageRoots_PublisherCaseDiffersFromHint(t *testing.T) {
	// The publisher directory capitalizes FFmpeg while the hint is lower
	// case. A case-sensitive glob misses this, which leaves the tool
	// undiscoverable even though it is installed.
	root := t.TempDir()
	want := writeFakeTool(t,
		filepath.Join(root, "Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe", "ffmpeg-9.0-full_build", "bin"),
		"ffmpeg")

	usePackageRoots(t, root)

	if got := searchPackageRoots(FFmpeg); got != want {
		t.Errorf("searchPackageRoots(FFmpeg) = %q, want %q", got, want)
	}
}

func TestSearchPackageRoots_Layouts(t *testing.T) {
	tests := []struct {
		name    string
		relDirs []string
	}{
		{name: "build dir then bin", relDirs: []string{"ffmpeg-9.0-full_build", "bin"}},
		{name: "bin directly under package", relDirs: []string{"bin"}},
		{name: "executable directly under package", relDirs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			parts := append([]string{root, "Gyan.FFmpeg_Source"}, tt.relDirs...)
			want := writeFakeTool(t, filepath.Join(parts...), "ffmpeg")

			usePackageRoots(t, root)

			if got := searchPackageRoots(FFmpeg); got != want {
				t.Errorf("searchPackageRoots(FFmpeg) = %q, want %q", got, want)
			}
		})
	}
}

func TestSearchPackageRoots_NoMatch(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		tool  Tool
	}{
		{
			name: "package matches the hint but holds no executable",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, "Gyan.FFmpeg_Source", "doc"), 0o755); err != nil {
					t.Fatalf("creating fixture: %v", err)
				}
			},
			tool: FFmpeg,
		},
		{
			name: "executable exists under a package the hint does not match",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeFakeTool(t, filepath.Join(root, "Unrelated.Package", "bin"), "ffmpeg")
			},
			tool: FFmpeg,
		},
		{
			name: "tool declares no hints",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeFakeTool(t, filepath.Join(root, "Streamlink.Package", "bin"), "streamlink")
			},
			tool: Streamlink,
		},
		{
			name:  "root does not exist",
			setup: func(*testing.T, string) {},
			tool:  FFmpeg,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)
			usePackageRoots(t, root)

			if got := searchPackageRoots(tt.tool); got != "" {
				t.Errorf("searchPackageRoots(%s) = %q, want empty", tt.tool.Name, got)
			}
		})
	}
}

// ///////////////////////////////////////////////
// locate
// ///////////////////////////////////////////////

func TestLocate_Precedence(t *testing.T) {
	t.Run("env override wins over PATH", func(t *testing.T) {
		envDir := t.TempDir()
		pathDir := t.TempDir()
		want := writeFakeTool(t, envDir, "ffmpeg")
		writeFakeTool(t, pathDir, "ffmpeg")

		tool := Tool{Name: "ffmpeg", EnvOverride: "STREAM_DVR_TEST_FFMPEG"}
		t.Setenv(tool.EnvOverride, want)
		t.Setenv("PATH", pathDir)
		usePackageRoots(t)

		got, source, err := locate(tool)
		if err != nil {
			t.Fatalf("locate() err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("locate() = %q, want %q", got, want)
		}
		if source != SourceEnv {
			t.Errorf("locate() source = %q, want %q", source, SourceEnv)
		}
	})

	t.Run("an override that does not resolve is refused", func(t *testing.T) {
		// An override is set to pin one copy of a tool, so the search it
		// replaces is a search the operator ruled out. Falling through to
		// it runs a different binary than the one named, which is the whole
		// thing the override was set to prevent.
		pathDir := t.TempDir()
		writeFakeTool(t, pathDir, "ffmpeg")
		pinned := filepath.Join(t.TempDir(), "absent")

		tool := Tool{Name: "ffmpeg", EnvOverride: "STREAM_DVR_TEST_FFMPEG"}
		t.Setenv(tool.EnvOverride, pinned)
		t.Setenv("PATH", pathDir)
		usePackageRoots(t)

		got, source, err := locate(tool)
		if err == nil {
			t.Fatalf("locate() = %q with err = nil, want the pinned path refused", got)
		}
		if got != "" {
			t.Errorf("locate() = %q, want empty", got)
		}
		if source != SourceEnv {
			t.Errorf("locate() source = %q, want %q so the report names the override", source, SourceEnv)
		}
		if !strings.Contains(err.Error(), pinned) {
			t.Errorf("locate() err = %q, want it to name the path that was pinned", err)
		}
	})

	t.Run("PATH wins over the package search", func(t *testing.T) {
		pathDir := t.TempDir()
		packageRoot := t.TempDir()
		want := writeFakeTool(t, pathDir, "ffmpeg")
		writeFakeTool(t, filepath.Join(packageRoot, "Gyan.FFmpeg_Source", "bin"), "ffmpeg")

		t.Setenv("PATH", pathDir)
		usePackageRoots(t, packageRoot)

		got, source, err := locate(FFmpeg)
		if err != nil {
			t.Fatalf("locate() err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("locate() = %q, want %q", got, want)
		}
		if source != SourcePath {
			t.Errorf("locate() source = %q, want %q", source, SourcePath)
		}
	})

	t.Run("package search is the last resort", func(t *testing.T) {
		packageRoot := t.TempDir()
		want := writeFakeTool(t, filepath.Join(packageRoot, "Gyan.FFmpeg_Source", "bin"), "ffmpeg")

		t.Setenv("PATH", t.TempDir())
		usePackageRoots(t, packageRoot)

		got, source, err := locate(FFmpeg)
		if err != nil {
			t.Fatalf("locate() err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("locate() = %q, want %q", got, want)
		}
		if source != SourceFallback {
			t.Errorf("locate() source = %q, want %q", source, SourceFallback)
		}
	})

	t.Run("nothing found", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		usePackageRoots(t)

		tool := Tool{Name: "stream-dvr-absent-tool", Hints: []string{"absent"}}
		got, source, err := locate(tool)
		if err != nil {
			t.Fatalf("locate() err = %v, want nil", err)
		}
		if got != "" {
			t.Errorf("locate() = %q, want empty", got)
		}
		if source != SourceMissing {
			t.Errorf("locate() source = %q, want %q", source, SourceMissing)
		}
	})
}

// ///////////////////////////////////////////////
// isExecutable
// ///////////////////////////////////////////////

func TestIsExecutable(t *testing.T) {
	// A candidate that is not a program has to be refused here, because
	// accepting one turns resolution into a spawn that fails at the moment
	// a broadcast starts. An environment override and a planted package
	// directory can both name a text file.
	windows := runtime.GOOS == "windows"
	dir := t.TempDir()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			name: "a program",
			path: writeFakeTool(t, dir, "ffmpeg"),
			want: true,
		},
		{
			// Windows carries no execute bit, so the name alone proves
			// nothing: a rename is all it takes. On Unix the mode is what
			// decides, and this fixture carries the exec bit.
			name: "a text file under a program's name",
			path: writeTextFile(t, dir, "planted"+exeExtension),
			want: !windows,
		},
		{
			name: "a text file under its own name",
			path: writeTextFile(t, dir, "notes.txt"),
			want: !windows,
		},
		{
			name: "a file with no execute permission",
			path: writeUnreadableMode(t, dir, "unrunnable"+exeExtension),
			want: windows,
		},
		{name: "directory", path: dir, want: false},
		{name: "missing path", path: filepath.Join(dir, "absent"), want: false},
		{name: "empty path", path: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExecutable(tt.path); got != tt.want {
				t.Errorf("isExecutable(%q) = %t, want %t", tt.path, got, tt.want)
			}
		})
	}
}

// writeUnreadableMode creates a file carrying an executable's header and
// name but no execute permission.
func writeUnreadableMode(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(peMagic+"-fake"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// ///////////////////////////////////////////////
// executableName
// ///////////////////////////////////////////////

func TestExecutableName(t *testing.T) {
	got := executableName("ffmpeg")
	want := "ffmpeg"
	if runtime.GOOS == "windows" {
		want = "ffmpeg.exe"
	}
	if got != want {
		t.Errorf("executableName(ffmpeg) = %q, want %q", got, want)
	}
}

// ///////////////////////////////////////////////
// Resolve
// ///////////////////////////////////////////////

func TestResolve_Missing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	usePackageRoots(t)

	tool := Tool{Name: "stream-dvr-absent-tool", Purpose: "prove absence is reported"}
	res := Resolve(context.Background(), tool)

	if !errors.Is(res.Err, ErrNotFound) {
		t.Errorf("Resolve() err = %v, want it to wrap ErrNotFound", res.Err)
	}
	if res.Source != SourceMissing {
		t.Errorf("Resolve() source = %q, want %q", res.Source, SourceMissing)
	}
	if res.Path != "" {
		t.Errorf("Resolve() path = %q, want empty", res.Path)
	}
	// The purpose belongs in the message so a failure says what broke.
	if !strings.Contains(res.Err.Error(), tool.Purpose) {
		t.Errorf("Resolve() err = %q, want it to mention the purpose %q", res.Err, tool.Purpose)
	}
}

func TestResolve_FoundWithoutVersionProbe(t *testing.T) {
	dir := t.TempDir()
	want := writeFakeTool(t, dir, "ffmpeg")

	// An empty VersionArg skips execution, so the fake file resolves
	// cleanly without being a runnable program.
	tool := Tool{Name: "ffmpeg", Purpose: "remux", EnvOverride: "STREAM_DVR_TEST_FFMPEG"}
	t.Setenv(tool.EnvOverride, want)
	usePackageRoots(t)

	res := Resolve(context.Background(), tool)
	if res.Err != nil {
		t.Fatalf("Resolve() err = %v, want nil", res.Err)
	}
	if res.Path != want {
		t.Errorf("Resolve() path = %q, want %q", res.Path, want)
	}
	if res.Version != "" {
		t.Errorf("Resolve() version = %q, want empty when no version arg is declared", res.Version)
	}
}

func TestResolve_RefusesAnOverrideThatDoesNotResolve(t *testing.T) {
	// Silently continuing to another copy is worse than failing: the
	// operator pinned a path precisely so the search that can be hijacked
	// is not used, and doctor would report the copy that was not chosen.
	pathDir := t.TempDir()
	writeFakeTool(t, pathDir, "ffmpeg")
	pinned := filepath.Join(t.TempDir(), "absent")

	tool := Tool{Name: "ffmpeg", Purpose: "remux", EnvOverride: "STREAM_DVR_TEST_FFMPEG"}
	t.Setenv(tool.EnvOverride, pinned)
	t.Setenv("PATH", pathDir)
	usePackageRoots(t)

	res := Resolve(context.Background(), tool)
	if res.Err == nil {
		t.Fatalf("Resolve() err = nil with path %q, want the pinned path refused", res.Path)
	}
	if res.Path != "" {
		t.Errorf("Resolve() path = %q, want empty rather than another copy of the tool", res.Path)
	}
}

func TestPath_ReturnsTheToolWhenOnlyTheVersionProbeFailed(t *testing.T) {
	// The probe runs the executable under a short deadline, and a machine
	// busy enough to miss it is a machine part way through the recording
	// the tool is about to be used for. Refusing the path there aborts a
	// capture over a banner.
	dir := t.TempDir()
	want := writeFakeTool(t, dir, "ffmpeg")

	tool := Tool{
		Name: "ffmpeg", Purpose: "remux", VersionArg: "-version",
		EnvOverride: "STREAM_DVR_TEST_FFMPEG",
	}
	t.Setenv(tool.EnvOverride, want)
	usePackageRoots(t)

	// The fake tool is not a runnable program, so the probe fails while the
	// file itself resolves.
	res := Resolve(context.Background(), tool)
	if res.Err == nil {
		t.Fatal("Resolve() err = nil, want the version failure kept so doctor still reports it")
	}

	got, err := Path(tool)
	if err != nil {
		t.Fatalf("Path() err = %v, want the resolved tool", err)
	}
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestResolve_ReadsTheVersionBanner(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want string
	}{
		{
			name: "an ordinary banner",
			mode: "banner",
			want: "9.0",
		},
		{
			// A prefix is a usable answer here, unlike one taken from a
			// body that is parsed: the version is the first line, and the
			// first line arrived whole. Refusing it would report a working
			// tool as broken over what it wrote afterwards.
			name: "a tool that keeps writing past its answer",
			mode: "flood",
			want: "9.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeFakeTool(t, dir, "ffmpeg")

			tool := Tool{
				Name: "ffmpeg", Purpose: "remux", VersionArg: "-version",
				EnvOverride: "STREAM_DVR_TEST_FFMPEG",
			}
			t.Setenv(tool.EnvOverride, path)
			usePackageRoots(t)
			fakeExec(t, tt.mode)

			res := Resolve(context.Background(), tool)
			if res.Err != nil {
				t.Fatalf("Resolve() err = %v, want nil", res.Err)
			}
			if res.Version != tt.want {
				t.Errorf("Resolve() version = %q, want %q", res.Version, tt.want)
			}
		})
	}
}

func TestResolve_BoundsAVersionWithNoEndToIt(t *testing.T) {
	// The banner is a prefix of whatever the tool wrote, so a run with no
	// whitespace in it yields a single field of that whole run. It lands in
	// a fixed column beside three other tools.
	dir := t.TempDir()
	path := writeFakeTool(t, dir, "ffmpeg")

	tool := Tool{
		Name: "ffmpeg", Purpose: "remux", VersionArg: "-version",
		EnvOverride: "STREAM_DVR_TEST_FFMPEG",
	}
	t.Setenv(tool.EnvOverride, path)
	usePackageRoots(t)
	fakeExec(t, "flood_one_run")

	res := Resolve(context.Background(), tool)
	if res.Err != nil {
		t.Fatalf("Resolve() err = %v, want nil", res.Err)
	}
	if res.Version == "" {
		t.Fatal("Resolve() version = empty, want what the tool managed to say")
	}
	if len(res.Version) > maxVersionText {
		t.Errorf("Resolve() version is %d bytes, want no more than %d", len(res.Version), maxVersionText)
	}
}

func TestNormalizeVersion_EscapesWhatTheToolPrinted(t *testing.T) {
	// The banner is chosen by a program on PATH and printed to a terminal
	// and a log, which is the one place every other value from outside is
	// escaped before it reaches.
	got := normalizeVersion("ffmpeg", "ffmpeg version 9.0\x1b[2Jcleared Copyright (c) 2026")
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("normalizeVersion() = %q, want the escape sequence rendered rather than emitted", got)
	}
}

func TestResolveAll_CoversEveryKnownTool(t *testing.T) {
	got := ResolveAll(context.Background())
	if len(got) != len(All) {
		t.Fatalf("ResolveAll() returned %d results, want %d", len(got), len(All))
	}
	for i, res := range got {
		if res.Tool.Name != All[i].Name {
			t.Errorf("ResolveAll()[%d] = %q, want %q", i, res.Tool.Name, All[i].Name)
		}
	}
}

func TestPath_NeverRunsTheTool(t *testing.T) {
	// Path is called on the way to every capture and every remux. Probing a
	// version there costs a subprocess per channel per poll, and a tool slow
	// to print its banner would abandon the recording it was clearing the
	// way for.
	dir := t.TempDir()
	want := writeFakeTool(t, dir, "ffmpeg")

	// A non-empty VersionArg is what makes Resolve spawn. The fixture is
	// not a runnable program, so anything that tried to run it would fail.
	tool := Tool{
		Name: "ffmpeg", Purpose: "remux", VersionArg: "-version",
		EnvOverride: "STREAM_DVR_TEST_FFMPEG",
	}
	t.Setenv(tool.EnvOverride, want)
	usePackageRoots(t)

	got, err := Path(tool)
	if err != nil {
		t.Fatalf("Path() err = %v, want the resolved tool with nothing spawned", err)
	}
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestLocate_ReportsTheSourceAlongsideThePath(t *testing.T) {
	dir := t.TempDir()
	want := writeFakeTool(t, dir, "ffmpeg")

	tool := Tool{Name: "ffmpeg", Purpose: "remux", EnvOverride: "STREAM_DVR_TEST_FFMPEG"}
	t.Setenv(tool.EnvOverride, want)
	usePackageRoots(t)

	got, source, err := Locate(tool)
	if err != nil {
		t.Fatalf("Locate() err = %v, want nil", err)
	}
	if got != want {
		t.Errorf("Locate() = %q, want %q", got, want)
	}
	if source != SourceEnv {
		t.Errorf("Locate() source = %q, want %q", source, SourceEnv)
	}
}

func TestLocate_ReportsAMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	usePackageRoots(t)

	got, source, err := Locate(Tool{Name: "stream-dvr-absent-tool", Purpose: "prove absence"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Locate() err = %v, want it to wrap ErrNotFound", err)
	}
	if got != "" {
		t.Errorf("Locate() = %q, want empty", got)
	}
	if source != SourceMissing {
		t.Errorf("Locate() source = %q, want %q", source, SourceMissing)
	}
}

// ///////////////////////////////////////////////
// Relative paths
// ///////////////////////////////////////////////

func TestLocate_NeverReturnsARelativePath(t *testing.T) {
	// An unset LOCALAPPDATA or ProgramFiles makes filepath.Join drop the
	// element and yield "Microsoft\WinGet\Links\ffmpeg.exe", which the glob
	// that follows resolves against the working directory. That directory
	// belongs to whoever started the daemon, not to this program.
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("ProgramFiles", "")
	t.Setenv("PATH", t.TempDir())

	// The real roots, so an empty variable is the only thing making a
	// pattern relative.
	original := packageRoots
	packageRoots = defaultPackageRoots
	t.Cleanup(func() { packageRoots = original })

	got, source, err := locate(FFmpeg)
	if err != nil {
		t.Fatalf("locate() err = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("locate() = %q, want empty rather than a path resolved against the working directory", got)
	}
	if source != SourceMissing {
		t.Errorf("locate() source = %q, want %q", source, SourceMissing)
	}
}

func TestLocate_RefusesARelativeOverride(t *testing.T) {
	// The override names the file the operator pinned. A relative one names
	// a different file in every directory the daemon might be started from.
	dir := t.TempDir()
	writeFakeTool(t, dir, "ffmpeg")

	tool := Tool{Name: "ffmpeg", Purpose: "remux", EnvOverride: "STREAM_DVR_TEST_FFMPEG"}
	t.Setenv(tool.EnvOverride, executableName("ffmpeg"))
	t.Chdir(dir)
	usePackageRoots(t)

	got, _, err := locate(tool)
	if err == nil {
		t.Fatalf("locate() = %q with err = nil, want a relative override refused", got)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("locate() err = %v, want it to wrap ErrNotFound", err)
	}
	if got != "" {
		t.Errorf("locate() = %q, want empty", got)
	}
}

func TestUnderEnv_DropsAnEntryWhoseVariableIsUnset(t *testing.T) {
	t.Setenv("STREAM_DVR_TEST_SET", filepath.FromSlash("/base"))
	t.Setenv("STREAM_DVR_TEST_UNSET", "")

	got := underEnv(
		envPath{env: "STREAM_DVR_TEST_UNSET", rest: []string{"tools", "ffmpeg"}},
		envPath{env: "STREAM_DVR_TEST_SET", rest: []string{"tools", "ffmpeg"}},
	)

	want := []string{filepath.Join(filepath.FromSlash("/base"), "tools", "ffmpeg")}
	if !slices.Equal(got, want) {
		t.Errorf("underEnv() = %v, want %v", got, want)
	}
}

// ///////////////////////////////////////////////
// Fallback locations
// ///////////////////////////////////////////////

func TestUnixPatterns_CoversHowThePythonToolsAreInstalled(t *testing.T) {
	// pipx puts both streamlink and yt-dlp under ~/.local/bin, which is how
	// a Python tool arrives on macOS and Linux. This list is the only thing
	// that finds a tool installed there and absent from PATH.
	got := unixPatterns(Streamlink)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	for _, want := range []string{
		filepath.Join(home, ".local", "bin", "streamlink"),
		filepath.Join(home, ".nix-profile", "bin", "streamlink"),
		filepath.Join("/opt/local/bin", "streamlink"),
		filepath.Join("/snap/bin", "streamlink"),
		filepath.Join("/var/lib/flatpak/exports/bin", "streamlink"),
		filepath.Join("/home/linuxbrew/.linuxbrew/bin", "streamlink"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("unixPatterns() = %v, want it to include %q", got, want)
		}
	}
}

func TestUnixPatterns_PrefersTheNativeHomebrewOverRosetta(t *testing.T) {
	// On Apple silicon /usr/local/bin holds the Rosetta x86_64 build and
	// /opt/homebrew/bin holds the native one. Taking the translated build
	// runs every transcode through emulation.
	got := unixPatterns(FFmpeg)

	native := slices.Index(got, filepath.Join("/opt/homebrew/bin", "ffmpeg"))
	rosetta := slices.Index(got, filepath.Join("/usr/local/bin", "ffmpeg"))
	if native < 0 || rosetta < 0 {
		t.Fatalf("unixPatterns() = %v, want both Homebrew prefixes", got)
	}
	if native > rosetta {
		t.Errorf("unixPatterns() puts /usr/local/bin before /opt/homebrew/bin, want the native build first")
	}
}

func TestPath_ReturnsErrorForMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	usePackageRoots(t)

	got, err := Path(Tool{Name: "stream-dvr-absent-tool", Purpose: "prove absence"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Path() err = %v, want it to wrap ErrNotFound", err)
	}
	if got != "" {
		t.Errorf("Path() = %q, want empty", got)
	}
}

func TestNormalizeVersion_SurvivesABannerWhoseCaseFoldingChangesItsLength(t *testing.T) {
	// Every byte here came from a program on PATH printing its banner, and
	// this function exists to survive that. strings.ToLower is not
	// length-preserving: U+023A is two bytes and lowers to a three-byte
	// rune, so an index taken from a lowered copy can point past the end of
	// the original it slices. Two such runes ahead of the marker take
	// doctor down with a stack trace; one makes it cut in the wrong place
	// and report a version the tool never stated.
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "an ordinary banner", line: "ffmpeg version 9.0", want: "9.0"},
		{name: "an uppercase marker", line: "ffmpeg VERSION 9.0", want: "9.0"},
		{name: "one rune that lengthens when lowered", line: "Ⱥ version 1.2.3", want: "1.2.3"},
		{name: "two of them", line: "ȺȺ version 1.2.3", want: "1.2.3"},
		{name: "ten of them", line: strings.Repeat("Ⱥ", 10) + " version 1.2.3", want: "1.2.3"},
		{name: "one before a long tail", line: "Ⱥ version 1.2.3.4.5.6", want: "1.2.3.4.5.6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeVersion("ffmpeg", tt.line); got != tt.want {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}
