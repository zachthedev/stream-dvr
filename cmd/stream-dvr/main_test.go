package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/secret"
	"zach.tools/go/stream-dvr/internal/version"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// found builds a successful resolution for a fake tool.
func found(name, version string, source deps.Source) deps.Resolution {
	return deps.Resolution{
		Tool:    deps.Tool{Name: name, Purpose: "do " + name + " things"},
		Path:    filepath.Join("/opt", name),
		Version: version,
		Source:  source,
	}
}

// missing builds a failed resolution for a fake tool.
func missing(name string) deps.Resolution {
	tool := deps.Tool{Name: name, Purpose: "do " + name + " things"}
	return deps.Resolution{
		Tool:   tool,
		Source: deps.SourceMissing,
		Err:    fmt.Errorf("%s (%s): %w", tool.Name, tool.Purpose, deps.ErrNotFound),
	}
}

// staticResolver returns a resolveFunc yielding a fixed result set, so
// doctor's reporting is testable without any tool installed.
func staticResolver(results ...deps.Resolution) resolveFunc {
	return func(context.Context) []deps.Resolution { return results }
}

// ///////////////////////////////////////////////
// runDoctor
// ///////////////////////////////////////////////

func TestRunDoctor_Outcomes(t *testing.T) {
	tests := []struct {
		name        string
		resolutions []deps.Resolution
		wantErr     bool
		wantText    []string
	}{
		{
			name: "every tool present",
			resolutions: []deps.Resolution{
				found("streamlink", "8.4.0", deps.SourcePath),
				found("ffmpeg", "9.0", deps.SourceFallback),
			},
			wantErr:  false,
			wantText: []string{"checks, all passed", "streamlink", "8.4.0", "ffmpeg", "fallback"},
		},
		{
			name: "one tool missing",
			resolutions: []deps.Resolution{
				found("streamlink", "8.4.0", deps.SourcePath),
				missing("ffmpeg"),
			},
			wantErr:  true,
			wantText: []string{"checks, 1 failed", "ffmpeg", "not found"},
		},
		{
			name: "several tools missing",
			resolutions: []deps.Resolution{
				missing("streamlink"),
				missing("ffmpeg"),
				missing("ffprobe"),
			},
			wantErr:  true,
			wantText: []string{"checks, 3 failed"},
		},
		{
			name:        "no tools declared",
			resolutions: nil,
			wantErr:     false,
			wantText:    []string{"checks, all passed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			err := runDoctor(context.Background(), &out, staticResolver(tt.resolutions...), noEncoders, "", "", false)

			if tt.wantErr && err == nil {
				t.Error("runDoctor() err = nil, want an error so the shell sees a non-zero exit")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("runDoctor() err = %v, want nil", err)
			}
			for _, want := range tt.wantText {
				if !strings.Contains(out.String(), want) {
					t.Errorf("runDoctor() output missing %q\ngot:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestRunDoctor_ReportsEveryTool(t *testing.T) {
	// The renderer clips a borderless table's last row unless the section
	// helper compensates, which silently hides a missing dependency.
	resolutions := []deps.Resolution{
		found("streamlink", "8.4.0", deps.SourcePath),
		found("ffmpeg", "9.0", deps.SourcePath),
		found("ffprobe", "9.0", deps.SourcePath),
		found("yt-dlp", "2026.07.04", deps.SourcePath),
	}

	var out bytes.Buffer
	if err := runDoctor(context.Background(), &out, staticResolver(resolutions...), noEncoders, "", "", false); err != nil {
		t.Fatalf("runDoctor() err = %v, want nil", err)
	}

	for _, res := range resolutions {
		if !strings.Contains(out.String(), res.Tool.Name) {
			t.Errorf("runDoctor() output missing %q\ngot:\n%s", res.Tool.Name, out.String())
		}
	}
}

func TestRunDoctor_AlwaysReportsLocations(t *testing.T) {
	var out bytes.Buffer
	if err := runDoctor(context.Background(), &out, staticResolver(), noEncoders, "", "", false); err != nil {
		t.Fatalf("runDoctor() err = %v, want nil", err)
	}

	dataDir := paths.DataDir()
	for _, want := range []string{"data dir", dataDir, paths.ConfigPath(dataDir)} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("runDoctor() output missing %q\ngot:\n%s", want, out.String())
		}
	}
}

func TestRunDoctor_Verbose(t *testing.T) {
	resolution := found("ffmpeg", "9.0", deps.SourcePath)

	t.Run("path hidden by default", func(t *testing.T) {
		var out bytes.Buffer
		if err := runDoctor(context.Background(), &out, staticResolver(resolution), noEncoders, "", "", false); err != nil {
			t.Fatalf("runDoctor() err = %v, want nil", err)
		}
		if strings.Contains(out.String(), resolution.Path) {
			t.Errorf("runDoctor() showed the path without --verbose\ngot:\n%s", out.String())
		}
	})

	t.Run("a fallback names its file without asking", func(t *testing.T) {
		// PATH and an override are places an operator put the tool. A
		// fallback is a package directory this search picked out of
		// several, so the file it settled on is shown without anyone
		// asking twice.
		fallback := found("ffmpeg", "9.0", deps.SourceFallback)

		var out bytes.Buffer
		if err := runDoctor(context.Background(), &out, staticResolver(fallback), noEncoders, "", "", false); err != nil {
			t.Fatalf("runDoctor() err = %v, want nil", err)
		}
		if !strings.Contains(out.String(), fallback.Path) {
			t.Errorf("runDoctor() hid the fallback's path\ngot:\n%s", out.String())
		}
	})

	t.Run("path shown when verbose", func(t *testing.T) {
		var out bytes.Buffer
		if err := runDoctor(context.Background(), &out, staticResolver(resolution), noEncoders, "", "", true); err != nil {
			t.Fatalf("runDoctor() err = %v, want nil", err)
		}
		if !strings.Contains(out.String(), resolution.Path) {
			t.Errorf("runDoctor() omitted the path with --verbose\ngot:\n%s", out.String())
		}
	})
}

func TestRunDoctor_Library(t *testing.T) {
	t.Run("owned library passes", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "library")
		if _, err := library.Create(root, "test"); err != nil {
			t.Fatalf("seeding library: %v", err)
		}

		var out bytes.Buffer
		err := runDoctor(context.Background(), &out, staticResolver(), noEncoders, "", root, false)
		if err != nil {
			t.Errorf("runDoctor() err = %v, want nil", err)
		}
		if !strings.Contains(out.String(), root) {
			t.Errorf("runDoctor() output missing the library root\ngot:\n%s", out.String())
		}
	})

	t.Run("unmarked directory fails", func(t *testing.T) {
		var out bytes.Buffer
		err := runDoctor(context.Background(), &out, staticResolver(), noEncoders, "", t.TempDir(), false)
		if err == nil {
			t.Error("runDoctor() err = nil, want a failure for a directory that is not a library")
		}
		if !strings.Contains(out.String(), "checks, 1 failed") {
			t.Errorf("runDoctor() output missing the failure count\ngot:\n%s", out.String())
		}
	})
}

// ///////////////////////////////////////////////
// initLibrary
// ///////////////////////////////////////////////

// configIn returns a config path inside a fresh temporary directory, so a
// test never reads or writes the config of the machine running it.
func configIn(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "config.toml")
}

func TestInitLibrary(t *testing.T) {
	t.Run("init creates a library", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "library")

		var out bytes.Buffer
		if err := initLibrary(&out, configIn(t), root, false, false); err != nil {
			t.Fatalf("initLibrary() err = %v, want nil", err)
		}
		if _, err := library.Open(root); err != nil {
			t.Errorf("Open() after init err = %v, want nil", err)
		}
		if !strings.Contains(out.String(), root) {
			t.Errorf("initLibrary() output missing the root\ngot: %s", out.String())
		}
	})

	t.Run("adopt claims an existing directory", func(t *testing.T) {
		root := t.TempDir()

		var out bytes.Buffer
		if err := initLibrary(&out, configIn(t), root, true, false); err != nil {
			t.Fatalf("initLibrary() err = %v, want nil", err)
		}
		if _, err := library.Open(root); err != nil {
			t.Errorf("Open() after adopt err = %v, want nil", err)
		}
	})

	t.Run("empty path is rejected", func(t *testing.T) {
		var out bytes.Buffer
		if err := initLibrary(&out, configIn(t), "", false, false); err == nil {
			t.Error("initLibrary() err = nil, want a rejection for an empty path")
		}
	})

	t.Run("running it twice is not an error", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "library")
		configPath := configIn(t)

		var out bytes.Buffer
		if err := initLibrary(&out, configPath, root, false, false); err != nil {
			t.Fatalf("first initLibrary() err = %v, want nil", err)
		}
		if err := initLibrary(&out, configPath, root, false, false); err != nil {
			t.Errorf("second initLibrary() err = %v, want the existing library accepted", err)
		}
	})
}

func TestInitLibrary_PointsAtALibraryThatAlreadyExists(t *testing.T) {
	// The state every machine running an older build is in: a library on
	// disk, made by a command that could not write the config, and a config
	// that never learned about it. A command that refuses here leaves that
	// operator hand-editing the file, which is the loop this whole change
	// exists to end.
	for _, adopt := range []bool{false, true} {
		name := "init"
		if adopt {
			name = "adopt"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "vods")
			if _, err := library.Create(root, "an earlier build"); err != nil {
				t.Fatalf("seeding the library: %v", err)
			}
			configPath := configIn(t)
			if err := config.Init(configPath); err != nil {
				t.Fatalf("seeding the config: %v", err)
			}

			var out bytes.Buffer
			if err := initLibrary(&out, configPath, root, adopt, false); err != nil {
				t.Fatalf("initLibrary() err = %v, want the existing library accepted", err)
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("config.Load() err = %v, want a config that needs no hand-edit", err)
			}
			if cfg.Library.Root != root {
				t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, root)
			}
			if !strings.Contains(out.String(), "already") {
				t.Errorf("initLibrary() reported a library it did not create as new\ngot: %s", out.String())
			}
		})
	}
}

func TestInitLibrary_StillRefusesTheOtherBuildsLibrary(t *testing.T) {
	// Accepting an existing library must not become accepting anyone's. A
	// build that opened the other lineage's library would write recordings
	// into a layout it does not own.
	root := filepath.Join(t.TempDir(), "vods")
	if err := os.MkdirAll(paths.StateDir(root), 0o755); err != nil {
		t.Fatalf("seeding the state directory: %v", err)
	}
	other := library.OwnerDev
	if library.BuildOwner == library.OwnerDev {
		other = library.OwnerProd
	}
	marker := fmt.Sprintf(`{"schema_version":1,"owner":%q,"created_at":%q,"created_by":"another build"}`,
		other, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(paths.MarkerPath(root), []byte(marker), 0o644); err != nil {
		t.Fatalf("seeding the marker: %v", err)
	}
	configPath := configIn(t)

	var out bytes.Buffer
	err := initLibrary(&out, configPath, root, false, false)
	if err == nil {
		t.Fatal("initLibrary() err = nil, want the other lineage's library refused")
	}
	if _, ok := errors.AsType[*library.OwnershipError](err); !ok {
		t.Errorf("err = %v, want an *OwnershipError", err)
	}

	got, rootErr := config.LibraryRoot(configPath)
	if rootErr != nil {
		t.Fatalf("config.LibraryRoot() err = %v", rootErr)
	}
	if got != "" {
		t.Errorf("Library.Root = %q, want a refused library left out of the config", got)
	}
}

func TestInitLibrary_RefusesARootTheConfigCouldNotHold(t *testing.T) {
	// A root holding a control or invisible character is one 'config
	// validate' refuses, so writing it would trade the missing-root failure
	// for an unfixable one, with a library on disk to go with it.
	base := t.TempDir()
	root := filepath.Join(base, "vo\u200bds")
	configPath := configIn(t)

	var out bytes.Buffer
	err := initLibrary(&out, configPath, root, false, false)
	if err == nil {
		t.Fatal("initLibrary() err = nil, want a root the config refuses rejected")
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) = %v, want no library created for a refused root", root, statErr)
	}
}

func TestInitLibrary_RefusalRendersOnOneLine(t *testing.T) {
	// main wraps every error in escape.Text, which quotes the whole string
	// once it holds a newline. A hint inside the message would reach the
	// operator as \n inside one long quoted line, with every path separator
	// doubled.
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	configPath := configIn(t)

	var out bytes.Buffer
	if err := initLibrary(&out, configPath, first, false, false); err != nil {
		t.Fatalf("seeding the first library: %v", err)
	}

	err := initLibrary(&out, configPath, second, false, false)
	if err == nil {
		t.Fatal("initLibrary() err = nil, want a refusal")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal carries a newline, so escape.Text quotes it whole: %q", err)
	}
	if rendered := escape.Text(err.Error()); strings.HasPrefix(rendered, `"`) {
		t.Errorf("the refusal renders quoted rather than as text: %s", rendered)
	}
}

func TestInitLibrary_LeavesAConfigThatValidatePasses(t *testing.T) {
	// The bug this covers cost the first operator their first four commands:
	// library init reported success and config validate then told them to
	// create a library, which is the command they had just run.
	for _, adopt := range []bool{false, true} {
		name := "init"
		if adopt {
			name = "adopt"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if !adopt {
				root = filepath.Join(root, "library")
			}
			configPath := configIn(t)
			if err := config.Init(configPath); err != nil {
				t.Fatalf("seeding the config: %v", err)
			}

			var out bytes.Buffer
			if err := initLibrary(&out, configPath, root, adopt, false); err != nil {
				t.Fatalf("initLibrary() err = %v, want nil", err)
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatalf("config.Load() err = %v, want a config that needs no hand-edit", err)
			}
			if cfg.Library.Root != root {
				t.Errorf("Library.Root = %q, want the library just created at %q", cfg.Library.Root, root)
			}
			if !strings.Contains(out.String(), configPath) {
				t.Errorf("initLibrary() never said the config was written\ngot: %s", out.String())
			}
		})
	}
}

func TestInitLibrary_WritesTheConfigOnAMachineThatHasNone(t *testing.T) {
	// A first run reaches library init before config init as often as after
	// it, and erroring here would send the operator to run one command so
	// they could repeat the one that just worked.
	root := filepath.Join(t.TempDir(), "library")
	configPath := configIn(t)

	var out bytes.Buffer
	if err := initLibrary(&out, configPath, root, false, false); err != nil {
		t.Fatalf("initLibrary() err = %v, want nil", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want a usable config", err)
	}
	if cfg.Library.Root != root {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, root)
	}
}

func TestInitLibrary_ResolvesARelativeRootBeforeRecordingIt(t *testing.T) {
	// The config refuses a relative root, so recording one verbatim would
	// trade the missing-root failure for an is-not-absolute failure.
	base := t.TempDir()
	t.Chdir(base)
	configPath := configIn(t)

	var out bytes.Buffer
	if err := initLibrary(&out, configPath, "library", false, false); err != nil {
		t.Fatalf("initLibrary() err = %v, want nil", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want a config that loads", err)
	}
	if !filepath.IsAbs(cfg.Library.Root) {
		t.Errorf("Library.Root = %q, want an absolute path", cfg.Library.Root)
	}
	if !paths.SameRoot(cfg.Library.Root, filepath.Join(base, "library")) {
		t.Errorf("Library.Root = %q, want the library created under %q", cfg.Library.Root, base)
	}
}

func TestInitLibrary_RefusesToRepointAConfigAtAnotherLibrary(t *testing.T) {
	// Repointing silently orphans the first library and every recording in
	// it, and the operator's next calendar is empty with nothing saying why.
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	configPath := configIn(t)

	var out bytes.Buffer
	if err := initLibrary(&out, configPath, first, false, false); err != nil {
		t.Fatalf("seeding the first library: %v", err)
	}

	err := initLibrary(&out, configPath, second, false, false)
	if err == nil {
		t.Fatal("initLibrary() err = nil, want a refusal rather than a silent repoint")
	}
	for _, want := range []string{first, second} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}

	// The refusal comes before anything is created, so a run the operator
	// did not mean leaves no second library to find and clean up.
	if _, statErr := os.Stat(second); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) = %v, want the refused library never created", second, statErr)
	}

	cfg, loadErr := config.Load(configPath)
	if loadErr != nil {
		t.Fatalf("config.Load() err = %v, want nil", loadErr)
	}
	if cfg.Library.Root != first {
		t.Errorf("Library.Root = %q, want the config still pointing at %q", cfg.Library.Root, first)
	}
}

func TestInitLibrary_RepointsUnderForce(t *testing.T) {
	// The refusal has to be answerable. An operator who moved their library
	// to another volume needs one command, not a hand-edit.
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	configPath := configIn(t)

	var out bytes.Buffer
	if err := initLibrary(&out, configPath, first, false, false); err != nil {
		t.Fatalf("seeding the first library: %v", err)
	}
	if err := initLibrary(&out, configPath, second, false, true); err != nil {
		t.Fatalf("initLibrary() with force err = %v, want nil", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want nil", err)
	}
	if cfg.Library.Root != second {
		t.Errorf("Library.Root = %q, want %q", cfg.Library.Root, second)
	}
}

func TestInitLibrary_KeepsTheChannelsTheConfigAlreadyLists(t *testing.T) {
	// An operator can list channels before they point at a library, and a
	// command that rewrites the config must not be how they lose that work.
	root := filepath.Join(t.TempDir(), "library")
	configPath := configIn(t)
	seed := config.DefaultConfig()
	seed.Channels = []config.Channel{{Platform: "twitch", Name: "examplechannel", Enabled: true}}
	if err := config.Save(configPath, seed); err != nil {
		t.Fatalf("seeding the config: %v", err)
	}

	var out bytes.Buffer
	if err := initLibrary(&out, configPath, root, false, false); err != nil {
		t.Fatalf("initLibrary() err = %v, want nil", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want nil", err)
	}
	if len(cfg.Channels) != 1 || cfg.Channels[0].Name != "examplechannel" {
		t.Errorf("Channels = %+v, want the one the file already listed", cfg.Channels)
	}
}

// ///////////////////////////////////////////////
// metadataRow
// ///////////////////////////////////////////////

func TestMetadataRow_NeverFailsACheck(t *testing.T) {
	// The metadata API is an optimisation: a machine without it records and
	// recovers identically, only spending one request per past broadcast
	// instead of one per channel. A doctor that failed over it would teach
	// the operator to ignore doctor.
	//
	// The glyph is the whole assertion, because the caller counts failures
	// by looking for the failure glyph.
	for _, id := range []string{"", "exampleclientid"} {
		got := metadataRow(id, nil, t.TempDir(), time.Now())
		if got.State == outcomeFail {
			t.Errorf("client id %q gave a failing row %q, want a note or a pass", id, joined(got))
		}
	}
}

func TestMetadataRow_SendsTheOperatorToRegisterAnApplication(t *testing.T) {
	// The id is config rather than something a build carries, so an install
	// without one is a step the operator has not taken yet. Naming only the
	// symptom would leave them with a report and no way to act on it.
	got := metadataRow("", nil, t.TempDir(), time.Now())

	if !strings.Contains(got.Trailer, "application id") {
		t.Errorf("row = %q, want it to name what is missing", got.Trailer)
	}
	if !strings.Contains(got.Trailer, "dev.twitch.tv/console/apps") {
		t.Errorf("row = %q, want it to name where an application is registered", got.Trailer)
	}
	if !strings.Contains(got.Trailer, "twitch.client_id") {
		t.Errorf("row = %q, want it to name the setting that holds the id", got.Trailer)
	}
	// Authorizing cannot help before an application exists, so the auth
	// command would send them at the wrong step.
	if strings.Contains(got.Trailer, "auth twitch metadata") {
		t.Errorf("row = %q, want the registration step rather than the authorize step", got.Trailer)
	}
}

func TestMetadataRow_SeparatesAnUnreadableConfigFromAnUnregisteredApplication(t *testing.T) {
	// A config that will not load zeroes every field it would have set, so
	// an id the operator really did paste in reads as absent. Reporting
	// that as an unregistered application sends them to Twitch to fix a
	// relative library.root, and doctor is the command they ran to find out
	// what was actually wrong.
	got := metadataRow("", errors.New("library.root: must be an absolute path"), t.TempDir(), time.Now())

	if !strings.Contains(got.Trailer, "config") {
		t.Errorf("row = %q, want it to name the config as the problem", got.Trailer)
	}
	if strings.Contains(got.Trailer, "dev.twitch.tv") {
		t.Errorf("row = %q, want no registration instruction for a config that did not load", got.Trailer)
	}
}

func TestMetadataRow_ReportsAnUnregisteredApplicationWhenTheConfigLoaded(t *testing.T) {
	// The other half of the pair. A config that loaded and names no id is
	// the case where registering one is exactly the right instruction, so
	// the two must not collapse into one message.
	got := metadataRow("", nil, t.TempDir(), time.Now())

	if !strings.Contains(got.Trailer, "dev.twitch.tv") {
		t.Errorf("row = %q, want the registration instruction", got.Trailer)
	}
	if strings.Contains(got.Trailer, "config validate") {
		t.Errorf("row = %q, want no config instruction when the config loaded", got.Trailer)
	}
}

func TestMetadataRow_PointsAtTheCommandWhenAuthorizingWouldHelp(t *testing.T) {
	// A build that can use a session, on a machine with none, is the one case
	// where the operator has something to do.
	got := metadataRow("exampleclientid", nil, t.TempDir(), time.Now())
	if !strings.Contains(got.Trailer, "auth twitch metadata") {
		t.Errorf("row = %q, want it to name the command that fixes this", got.Trailer)
	}
}

func TestMetadataRow_ReadsAnExpiredAccessTokenAsOrdinary(t *testing.T) {
	// An access token lives about four hours and is renewed from the refresh
	// half on next use, so between recovery passes it is usually expired.
	// Reporting that as a problem would cry wolf on every idle machine.
	dataDir := t.TempDir()

	session := twitch.NewSession("exampleclientid", secret.NewFile(dataDir), secret.AccountTwitchAPI, nil)
	if err := session.Authorize(twitch.Tokens{
		Access: "EXAMPLEACCESSEXAMPLEACCESS1234", Refresh: "EXAMPLEREFRESHEXAMPLEREFRESH12",
		ExpiresIn: time.Hour,
	}); err != nil {
		t.Fatalf("Authorize() err = %v, want nil", err)
	}

	got := metadataRow("exampleclientid", nil, dataDir, time.Now().Add(2*time.Hour))
	if got.State == outcomeFail {
		t.Errorf("row = %q, want an expired access token to read as ordinary", joined(got))
	}
	if !strings.Contains(got.Trailer, "authorized") {
		t.Errorf("row = %q, want it to say the session is authorized", got.Trailer)
	}
}

func TestMetadataRow_CarriesNoCredential(t *testing.T) {
	// doctor output is the kind of thing pasted into a bug report.
	dataDir := t.TempDir()

	const access = "EXAMPLEACCESSEXAMPLEACCESS1234"
	const refresh = "EXAMPLEREFRESHEXAMPLEREFRESH12"
	session := twitch.NewSession("exampleclientid", secret.NewFile(dataDir), secret.AccountTwitchAPI, nil)
	if err := session.Authorize(twitch.Tokens{
		Access: access, Refresh: refresh, ExpiresIn: 4 * time.Hour,
	}); err != nil {
		t.Fatalf("Authorize() err = %v, want nil", err)
	}

	rendered := joined(metadataRow("exampleclientid", nil, dataDir, time.Now()))
	for _, credential := range []string{access, refresh} {
		if strings.Contains(rendered, credential) {
			t.Errorf("the row carries a credential: %q", rendered)
		}
	}
}

// ///////////////////////////////////////////////
// versionLine
// ///////////////////////////////////////////////

func TestVersionLine_NamesTheBinaryAndItsVersion(t *testing.T) {
	// The two facts an operator asks this command for.
	line := versionLine()

	if !strings.HasPrefix(line, paths.BinaryName+" ") {
		t.Errorf("versionLine() = %q, want it to open with the binary name", line)
	}
	if !strings.Contains(line, version.Info()) {
		t.Errorf("versionLine() = %q, want it to carry %q", line, version.Info())
	}
}

func TestVersionLine_CallsOutOnlyTheSandboxLineage(t *testing.T) {
	// A build tagged dev refuses to open a library a released binary made,
	// and an operator holding one without knowing it reads that refusal as a
	// damaged library. The ordinary build says nothing, because claiming to
	// be a "prod build" beside a version reading 0.0.0-dev is a
	// contradiction an operator has to stop and resolve.
	line := versionLine()

	if library.BuildOwner == library.OwnerDev {
		if !strings.Contains(line, "dev sandbox") {
			t.Errorf("versionLine() = %q, want a dev build to say it refuses a real library", line)
		}
		return
	}
	if strings.Contains(line, "build)") {
		t.Errorf("versionLine() = %q, want no lineage note on an ordinary build", line)
	}
}

// ///////////////////////////////////////////////
// plural
// ///////////////////////////////////////////////

func TestPlural(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want string
	}{
		{name: "zero takes the plural", n: 0, want: "checks failed"},
		{name: "one takes the singular", n: 1, want: "check failed"},
		{name: "many take the plural", n: 2, want: "checks failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plural(tt.n, "check failed", "checks failed"); got != tt.want {
				t.Errorf("plural(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// newApp
// ///////////////////////////////////////////////

func TestNewApp_RegistersCommands(t *testing.T) {
	app := newApp()

	want := map[string]bool{"version": false, "doctor": false, "library": false}
	for _, cmd := range app.Commands {
		if _, ok := want[cmd.Name]; ok {
			want[cmd.Name] = true
		}
	}
	for name, registered := range want {
		if !registered {
			t.Errorf("newApp() is missing the %q command", name)
		}
	}
}

// run drives the whole command tree the way main does, with output
// discarded so a usage error does not reach the test log.
func run(t *testing.T, args ...string) error {
	t.Helper()

	app := newApp()
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	return app.Run(context.Background(), append([]string{paths.BinaryName}, args...))
}

// isolateDataDir points the default config lookup at an empty directory, so
// a test that reaches the config never finds the operator's real one and
// opens their library.
func isolateDataDir(t *testing.T) {
	t.Helper()
	t.Setenv(paths.EnvDataDir, t.TempDir())
}

func TestNewApp_RootIsTheInterface(t *testing.T) {
	// The interface is what running the binary means, so it must not also
	// sit behind a subcommand name.
	for _, cmd := range newApp().Commands {
		for _, name := range append([]string{cmd.Name}, cmd.Aliases...) {
			if name == "tui" || name == "ui" {
				t.Errorf("newApp() still registers %q; the root command is the interface", name)
			}
		}
	}
}

func TestNewApp_RootRunsTheInterface(t *testing.T) {
	// Reaching the config load proves the root ran the interface rather than
	// printing help.
	isolateDataDir(t)

	err := run(t)
	if !errors.Is(err, config.ErrNotFound) {
		t.Errorf("run() err = %v, want it to reach the config load and report %v", err, config.ErrNotFound)
	}
}

func TestNewApp_RejectsAnUnknownCommand(t *testing.T) {
	// Any unmatched argument falls through to the root action, so without
	// a guard a typo would open the calendar as if nothing had been typed.
	isolateDataDir(t)

	err := run(t, "serv")
	if err == nil {
		t.Fatal("run() err = nil, want an unknown command to be refused")
	}
	for _, want := range []string{"serv", "--help"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("run() err = %q, want it to contain %q", err, want)
		}
	}
}

func TestNewApp_ConfigFlagReachesEveryLevel(t *testing.T) {
	// One flag has to mean one thing wherever it appears. A subcommand
	// declaring its own --config shadows the root's, which leaves a value
	// given ahead of the subcommand name parsed and then never read.
	missing := filepath.Join(t.TempDir(), "absent.toml")

	tests := []struct {
		name string
		args []string
	}{
		{name: "root", args: []string{"--config", missing}},
		{name: "before a subcommand", args: []string{"--config", missing, "serve"}},
		{name: "after a subcommand", args: []string{"serve", "--config", missing}},
		{name: "before config validate", args: []string{"--config", missing, "config", "validate"}},
		{name: "after config validate", args: []string{"config", "validate", "--config", missing}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateDataDir(t)

			err := run(t, tt.args...)
			if !errors.Is(err, config.ErrNotFound) {
				t.Fatalf("run(%v) err = %v, want it to read the flag and report %v", tt.args, err, config.ErrNotFound)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("run(%v) err = %q, want it to name %q", tt.args, err, missing)
			}
		})
	}
}

func TestNewApp_MondayIsRootOnly(t *testing.T) {
	// Only the calendar has weeks. A subcommand accepting the flag and
	// doing nothing with it would be worse than refusing it.
	isolateDataDir(t)

	err := run(t, "serve", "--monday")
	if err == nil {
		t.Fatal("run() err = nil, want serve to refuse a flag it cannot honor")
	}
	if !strings.Contains(err.Error(), "monday") {
		t.Errorf("run() err = %q, want it to name the flag", err)
	}
}

// ///////////////////////////////////////////////
// configFile
// ///////////////////////////////////////////////

func TestConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(paths.EnvDataDir, dataDir)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "falls back to the data directory",
			args: nil,
			want: paths.ConfigPath(dataDir),
		},
		{
			name: "an explicit flag wins",
			args: []string{"--config", filepath.Join("elsewhere", "custom.toml")},
			want: filepath.Join("elsewhere", "custom.toml"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			app := &cli.Command{
				Name:   paths.BinaryName,
				Flags:  newApp().Flags,
				Writer: io.Discard,
				Action: func(_ context.Context, cmd *cli.Command) error {
					got = configFile(cmd)
					return nil
				},
			}

			args := append([]string{paths.BinaryName}, tt.args...)
			if err := app.Run(context.Background(), args); err != nil {
				t.Fatalf("Run(%v) err = %v, want nil", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("configFile() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Arguments the tree has no use for
// ///////////////////////////////////////////////

func TestNewApp_RefusesAnArgumentNoCommandClaims(t *testing.T) {
	// A parent command with no action of its own falls through to urfave's
	// help-topic lookup, which prints "No help topic for 'bogus'" and exits
	// 3. An extra operand on a leaf is worse: it is silently ignored, so a
	// mistyped flag that landed as an operand reads as though it applied.
	//
	// Driven through the whole command tree, because what matters is which
	// argument reaches which action.
	//
	// install, uninstall, status and serve are absent on purpose. Running
	// their actions reaches the machine's real scheduler and its real
	// configuration, which is the one thing a test here must not do. A
	// rejection that failed to fire is exactly when that would happen.
	// TestNoOperands_GuardsEveryCommandThatReachesTheMachine covers those.
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "an unknown top-level command", args: []string{"bogus"}, want: "unknown command"},
		{name: "an unknown config command", args: []string{"config", "bogus"}, want: "unknown command"},
		{name: "an unknown library command", args: []string{"library", "bogus"}, want: "unknown command"},
		{name: "config with no subcommand", args: []string{"config"}, want: "needs a subcommand"},
		{name: "library with no subcommand", args: []string{"library"}, want: "needs a subcommand"},
		{name: "an operand on config path", args: []string{"config", "path", "extra"}, want: "takes no arguments"},
		{name: "an operand on version", args: []string{"version", "extra"}, want: "takes no arguments"},
		{name: "an operand on doctor", args: []string{"doctor", "extra"}, want: "takes no arguments"},
		{
			name: "a second operand on library init",
			args: []string{"library", "init", "one", "two"},
			want: "takes one library path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newApp()
			app.Writer = io.Discard
			app.ErrWriter = io.Discard

			err := app.Run(context.Background(), append([]string{paths.BinaryName}, tt.args...))
			if err == nil {
				t.Fatalf("Run(%v) err = nil, want a rejection", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Run(%v) err = %q, want it to say %q", tt.args, err, tt.want)
			}
		})
	}
}

func TestNoOperands_GuardsEveryCommandThatReachesTheMachine(t *testing.T) {
	// install, uninstall, status and serve cannot be run here: their actions
	// reach the machine's scheduler and its configuration. What can be checked
	// is that each one calls the guard, and that no command in the tree is
	// left without an action.
	guarded := map[string]bool{}
	for _, name := range []string{"install", "uninstall", "status", "serve"} {
		guarded[name] = false
	}

	source, err := os.ReadFile(filepath.Join("install.go"))
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	serve, err := os.ReadFile(filepath.Join("serve.go"))
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	source = append(source, serve...)

	for _, command := range newApp().Commands {
		if _, watched := guarded[command.Name]; !watched {
			continue
		}
		if command.Action == nil {
			t.Errorf("%s has no action, so urfave decides what an argument means", command.Name)
			continue
		}
		guarded[command.Name] = true
	}

	for name, wired := range guarded {
		if !wired {
			t.Errorf("%s is not in the command tree", name)
		}
	}
	if got := strings.Count(string(source), "noOperands(cmd)"); got != len(guarded) {
		t.Errorf("%d of the machine-touching commands call the operand guard, want %d", got, len(guarded))
	}
}

func TestNewApp_LeavesNoCommandWithoutAnAction(t *testing.T) {
	// A command with no action falls through to urfave's help-topic lookup,
	// which prints "No help topic for 'bogus'" at exit 3 and names neither
	// what was typed nor what would have been valid.
	var walk func(cmd *cli.Command)
	walk = func(cmd *cli.Command) {
		if cmd.Action == nil {
			t.Errorf("%s has no action", cmd.FullName())
		}
		for _, child := range cmd.Commands {
			walk(child)
		}
	}
	walk(newApp())
}

func TestUnknownCommand_EscapesWhatWasTyped(t *testing.T) {
	// The rejected argument is echoed back to a terminal, and it is whatever
	// the operator's shell handed over.
	app := newApp()
	app.Writer = io.Discard
	app.ErrWriter = io.Discard

	err := app.Run(context.Background(), []string{paths.BinaryName, "bogus\x1b[2Jname\a"})
	if err == nil {
		t.Fatal("Run() err = nil, want a rejection")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("the argument did not reach the message at all: %q", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\a") {
		t.Errorf("a control byte from an argument reached the message: %q", err)
	}
}

// ///////////////////////////////////////////////
// Untrusted text in CLI output
// ///////////////////////////////////////////////

func TestRunDoctor_ControlCharactersInAToolVersionNeverReachTheTerminal(t *testing.T) {
	// A tool's version is a line of that subprocess's stdout, and the
	// resolved path is whatever resolution found. The TUI escapes everything
	// it prints, and this is the same terminal.
	resolution := found("ffmpeg", "7.1\x1b[2Jfake\a", deps.SourcePath)
	resolution.Path = "/opt/ff\x1bmpeg"

	var out bytes.Buffer
	if err := runDoctor(context.Background(), &out, staticResolver(resolution), noEncoders, "", "", true); err != nil {
		t.Fatalf("runDoctor() err = %v, want nil", err)
	}

	if !strings.Contains(out.String(), "7.1") {
		t.Fatalf("the version did not render at all, so this proved nothing:\n%q", out.String())
	}
	if strings.ContainsAny(out.String(), "\x1b\a") {
		t.Errorf("a control byte from a subprocess reached the rendered output:\n%q", out.String())
	}
}

func TestInitLibrary_RefusesARootHoldingControlCharacters(t *testing.T) {
	// A control byte does not survive the trip downstream: url.PathEscape
	// writes it as %00 and a C API stops at it, so a library rooted on such
	// a path is one no config can name back. Refusing is the stronger
	// guarantee than rendering it safely, and it comes before anything is
	// created, so a root the config would reject leaves nothing on disk.
	//
	// Nothing is created here, which is what lets this run everywhere.
	// Windows refuses a control byte in a filename outright, so a test that
	// had to make the directory first could only run on Linux and macOS,
	// and the platforms it skipped are the ones CI checks.
	parent := t.TempDir()
	root := filepath.Join(parent, "lib\x1b[2Jrary")

	var out bytes.Buffer
	err := initLibrary(&out, configIn(t), root, false, false)

	if err == nil {
		t.Fatal("initLibrary() accepted a root holding an escape byte")
	}
	// The refusal has to say why, or the reason is free to disappear while
	// the refusal stays and still looks correct.
	if !strings.Contains(err.Error(), "control or invisible characters") {
		t.Errorf("initLibrary() err = %v, want it to name the reason", err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("an escape byte from a library root reached the rendered output:\n%q", out.String())
	}

	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatalf("reading the parent directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused root left %d entries on disk", len(entries))
	}
}

func TestDescribeWatch_ControlCharactersInAChannelNameNeverReachTheTerminal(t *testing.T) {
	// Channel names come from the config file, which is edited by hand and
	// travels with a library between machines.
	cfg := config.Config{Channels: []config.Channel{
		{Platform: "twitch", Name: "example\x1b]0;PWNED\achannel", Enabled: true},
	}}

	got := describeWatch(cfg)
	if !strings.Contains(got, "channel") {
		t.Fatalf("the channel did not render at all, so this proved nothing: %q", got)
	}
	if strings.ContainsAny(got, "\x1b\a") {
		t.Errorf("a control byte from a channel name reached the rendered output: %q", got)
	}
}

// ///////////////////////////////////////////////
// Recompress row
// ///////////////////////////////////////////////

// noEncoders answers the doctor's encoder probe with nothing, so no test
// spawns ffmpeg to find out what this machine can do.
func noEncoders(context.Context) ([]post.Encoder, error) {
	return nil, nil
}

// staticEncoders answers with a fixed set.
func staticEncoders(encoders ...post.Encoder) encodersFunc {
	return func(context.Context) ([]post.Encoder, error) { return encoders, nil }
}

func TestRecompressRow(t *testing.T) {
	// doctor is where the cross-platform answer reaches the operator. The
	// catalogue is a list of what exists somewhere. Which entry this machine
	// can run is a fact about the machine, and one broadcast costs under an
	// hour on hardware against six to sixteen in software.
	hardware := post.Encoder{Name: "hevc_nvenc", Codec: config.CodecHEVC, Hardware: true}
	software := post.Encoder{Name: "libx265", Codec: config.CodecHEVC, Hardware: false}

	tests := []struct {
		name       string
		recompress config.Recompress
		encoders   encodersFunc
		want       []string
		avoid      []string
	}{
		{
			name:       "a hardware encoder is available",
			recompress: config.Recompress{Enabled: true, Codec: config.CodecHEVC, PreferHardware: true},
			encoders:   staticEncoders(hardware, software),
			want:       []string{"recompress", "on", "hevc", "hevc_nvenc"},
			avoid:      []string{"in software"},
		},
		{
			// The setting's own documentation tells the operator to run
			// doctor before turning it on, so the row has to answer while
			// it is still off.
			name:       "reported while it is off",
			recompress: config.Recompress{Enabled: false, Codec: config.CodecHEVC, PreferHardware: true},
			encoders:   staticEncoders(hardware),
			want:       []string{"recompress", "off", "hevc_nvenc"},
		},
		{
			// The macOS and stock-Linux case. doctor answers before a pass
			// runs, where the daemon's software warning lands only after one
			// starts.
			name:       "only software, and hardware was preferred",
			recompress: config.Recompress{Enabled: true, Codec: config.CodecHEVC, PreferHardware: true},
			encoders:   staticEncoders(software),
			want:       []string{"libx265", "in software", "hours per broadcast"},
		},
		{
			name:       "no encoder can produce the configured codec",
			recompress: config.Recompress{Enabled: true, Codec: config.CodecAV1, PreferHardware: true},
			encoders:   staticEncoders(hardware, software),
			want:       []string{"no encoder on this machine can produce it"},
		},
		{
			// A machine with no ffmpeg is a machine that cannot answer the
			// question, which is not the same as failing a check.
			name:       "the probe itself fails",
			recompress: config.Recompress{Enabled: true, Codec: config.CodecHEVC},
			encoders: func(context.Context) ([]post.Encoder, error) {
				return nil, errors.New("ffmpeg not found")
			},
			want: []string{"encoders could not be probed", "ffmpeg not found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			built := recompressRow(context.Background(), tt.encoders, tt.recompress)
			got := joined(built)

			// Never a failure, and so never the passing mark either. Every
			// answer this row can give is a note.
			if built.State != outcomeNote {
				t.Errorf("recompressRow() state = %v, want a note", built.State)
			}

			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("recompressRow() = %q, want it to carry %q", got, want)
				}
			}
			for _, avoid := range tt.avoid {
				if strings.Contains(got, avoid) {
					t.Errorf("recompressRow() = %q, want it not to carry %q", got, avoid)
				}
			}
		})
	}
}

func TestRunDoctor_ReportsTheRecompressEncoder(t *testing.T) {
	// The row is worth nothing unless doctor prints it.
	var out bytes.Buffer
	encoders := staticEncoders(post.Encoder{Name: "hevc_nvenc", Codec: config.CodecHEVC, Hardware: true})

	if err := runDoctor(context.Background(), &out, staticResolver(), encoders, "", "", false); err != nil {
		t.Fatalf("runDoctor() err = %v, want nil", err)
	}

	if !strings.Contains(out.String(), "recompress") {
		t.Errorf("runDoctor() output has no recompress row\ngot:\n%s", out.String())
	}
}

func TestRunDoctor_AnUnprobeableEncoderIsNotAFailedCheck(t *testing.T) {
	// Recompress is off by default and a machine with no hardware encoder
	// is a supported machine. Failing doctor over it would tell an
	// operator their install is broken when it is not.
	var out bytes.Buffer
	encoders := func(context.Context) ([]post.Encoder, error) {
		return nil, errors.New("ffmpeg not found")
	}

	if err := runDoctor(context.Background(), &out, staticResolver(), encoders, "", "", false); err != nil {
		t.Errorf("runDoctor() err = %v, want an unanswerable probe reported rather than failed", err)
	}
}

func TestInitLibrary_LeavesAnExistingDatabaseAlone(t *testing.T) {
	// store.Open migrates, so opening a library that already carries a
	// database can move the schema underneath a daemon serving it. A file
	// that is not a database at all stands in for one this must not touch:
	// opening it would fail, so success is the proof it was left alone.
	root := filepath.Join(t.TempDir(), "vods")
	if _, err := library.Create(root, "an earlier build"); err != nil {
		t.Fatalf("seeding the library: %v", err)
	}
	lib, err := library.Open(root)
	if err != nil {
		t.Fatalf("opening the library: %v", err)
	}
	sentinel := []byte("this is not a database")
	if err := os.WriteFile(lib.DatabasePath(), sentinel, 0o600); err != nil {
		t.Fatalf("seeding the database path: %v", err)
	}

	var out bytes.Buffer
	if err := initLibrary(&out, configIn(t), root, false, false); err != nil {
		t.Fatalf("initLibrary() err = %v, want the existing database left alone", err)
	}

	after, err := os.ReadFile(lib.DatabasePath())
	if err != nil {
		t.Fatalf("reading the database path: %v", err)
	}
	if !bytes.Equal(after, sentinel) {
		t.Errorf("the database was rewritten, so a live daemon's schema would have moved")
	}
}

func TestInitLibrary_BuildsADatabaseForALibraryWithNone(t *testing.T) {
	// An older build made libraries without one, and the calendar opens as a
	// client that refuses to migrate. Nothing can be serving a library with
	// no database, so building it there is free.
	root := filepath.Join(t.TempDir(), "vods")
	if _, err := library.Create(root, "an earlier build"); err != nil {
		t.Fatalf("seeding the library: %v", err)
	}
	lib, err := library.Open(root)
	if err != nil {
		t.Fatalf("opening the library: %v", err)
	}
	if err := os.Remove(lib.DatabasePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clearing the database: %v", err)
	}

	var out bytes.Buffer
	if err := initLibrary(&out, configIn(t), root, false, false); err != nil {
		t.Fatalf("initLibrary() err = %v, want nil", err)
	}

	if _, err := os.Stat(lib.DatabasePath()); err != nil {
		t.Errorf("os.Stat(database) = %v, want one built for a library that had none", err)
	}
}

func TestRefuse_KeepsAMultiLineErrorReadable(t *testing.T) {
	// Every failure in the tree ends here. Escaping the whole message and
	// cutting the result splits inside whatever escape.Text produced, so a
	// message carrying a newline arrives as one quoted literal: the line
	// breaks print as backslash-n, Windows separators double into a path
	// nobody can copy, and the opening quote is never closed. A config with
	// two problems and a mistyped subcommand both carry one.
	var out bytes.Buffer
	refuse(&out, errors.New("invalid config, 2 problems:"+
		"\n  library.root: must be an absolute path"+
		"\n  capture.poll_interval: must be at least 5s"))

	got := out.String()
	if strings.Contains(got, `\n`) {
		t.Errorf("the line breaks printed as text:\n%s", got)
	}
	if !strings.Contains(got, "library.root") || !strings.Contains(got, "capture.poll_interval") {
		t.Errorf("a problem was lost:\n%s", got)
	}
}

func TestRefuse_DoesNotDoubleTheSeparatorsOfAWindowsPath(t *testing.T) {
	// The path is what an operator copies out of a refusal. Quoting the
	// whole message doubles every separator in it.
	var out bytes.Buffer
	refuse(&out, errors.New(`C:\Users\Example\config.toml: invalid config`+
		"\n  library.root: is required"))

	if got := out.String(); !strings.Contains(got, `C:\Users\Example\config.toml`) {
		t.Errorf("the path was not printed as it was written:\n%s", got)
	}
}

func TestRefuse_SplitsTheHintOnTheBuildsOwnSeparator(t *testing.T) {
	// The cause and the hint are one string across a package boundary, and
	// the semicolon is how this codebase writes the two halves.
	var out bytes.Buffer
	refuse(&out, errors.New("the library is not initialized; run stream-dvr library init"))

	got := out.String()
	if !strings.Contains(got, "the library is not initialized") {
		t.Errorf("the cause is missing:\n%s", got)
	}
	if !strings.Contains(got, "run stream-dvr library init") {
		t.Errorf("the hint is missing:\n%s", got)
	}
}
