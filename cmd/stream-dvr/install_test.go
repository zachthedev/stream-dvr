package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/service"
)

// ///////////////////////////////////////////////
// startupHint
// ///////////////////////////////////////////////

func TestSessionLimit_NamesTheOnePlatformThatStopsAtLogout(t *testing.T) {
	// Windows registers a scheduled task with a boot trigger and Linux a
	// lingering user unit, so both keep recording with nobody signed in.
	// macOS runs a launchd agent in the operator's own session, because a
	// root daemon cannot read the streamlink credentials in the home
	// directory. That trade costs recording at the login window, and the
	// operator has to be told at the moment they register it.
	limit := sessionLimit()

	if runtime.GOOS != "darwin" {
		if limit != "" {
			t.Errorf("sessionLimit() = %q on %s, want nothing", limit, runtime.GOOS)
		}
		return
	}
	if limit == "" {
		t.Fatal("sessionLimit() = \"\" on darwin, want the login-window limit named")
	}
	for _, want := range []string{"signed in", "login window"} {
		if !strings.Contains(limit, want) {
			t.Errorf("sessionLimit() = %q, want it to mention %q", limit, want)
		}
	}
}

func TestStartupHint(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHint bool
	}{
		{
			// The scheduler's own message says only that access was
			// denied, which does not tell an operator what to do.
			name:     "privilege refusal gains the fix",
			err:      service.ErrElevationRequired,
			wantHint: true,
		},
		{
			name: "unsupported platform is passed through",
			err:  service.ErrUnsupported,
		},
		{
			name: "an unrelated failure is passed through",
			err:  errors.New("scheduler is broken"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := startupHint(tt.err)

			if !errors.Is(got, tt.err) {
				t.Errorf("startupHint() = %v, want it to wrap the original", got)
			}
			// Keyed on the hint's own words. The refusal already says an
			// elevated shell is needed, so looking for that phrase would
			// pass whether or not anything was added to it.
			carriesHint := strings.Contains(got.Error(), "recording itself needs no elevation")
			if carriesHint != tt.wantHint {
				t.Errorf("startupHint() = %q, want the elevation hint present = %t",
					got, tt.wantHint)
			}
		})
	}
}

func TestStartupHint_SaysRecordingNeedsNoElevation(t *testing.T) {
	// Registering and removing each need one elevated prompt, and recording
	// needs none. Without that distinction an operator reasonably assumes the
	// recorder itself wants administrator rights.
	got := startupHint(service.ErrElevationRequired).Error()

	if !strings.Contains(got, "recording itself needs no elevation") {
		t.Errorf("startupHint() = %q, want the elevation scoped to the one command", got)
	}
}

// ///////////////////////////////////////////////
// Status output
// ///////////////////////////////////////////////

func TestRunStatus(t *testing.T) {
	// Status must work on a machine with nothing registered, which is
	// every machine before the first install.
	var out bytes.Buffer

	if err := runStatus(&out); err != nil {
		t.Fatalf("runStatus() err = %v, want nil", err)
	}
	if !strings.Contains(out.String(), serviceName) {
		t.Errorf("runStatus() = %q, want it to name the registration", out.String())
	}
}

func TestReportStatus(t *testing.T) {
	// A disabled registration is complete, triggered, and will never start.
	// Reported like any other installed one it reads as a recorder waiting
	// for its next broadcast, so the operator learns otherwise by finding a
	// day with nothing recorded on it.
	//
	// The glyph is asserted because it is the part read at a glance. A
	// sentence that survives while the marker reverts to the one every other
	// state carries puts the warning where nobody looks.
	tests := []struct {
		name   string
		state  service.State
		glyph  string
		want   string
		absent string
	}{
		{
			name:   "running",
			state:  service.StateRunning,
			glyph:  glyphOK,
			want:   "running",
			absent: "enabled again",
		},
		{
			name:   "installed",
			state:  service.StateInstalled,
			glyph:  glyphNote,
			want:   "installed",
			absent: "enabled again",
		},
		{
			name:   "absent",
			state:  service.StateAbsent,
			glyph:  glyphNote,
			want:   "absent",
			absent: "enabled again",
		},
		{
			name:   "disabled",
			state:  service.StateDisabled,
			glyph:  glyphWarn,
			want:   "it will not start until it is enabled again",
			absent: "installed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			reportStatus(styled(&out), service.Status{State: tt.state, Detail: serviceName}, "a mechanism")

			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("reportStatus() = %q, want it to carry %q", out.String(), tt.want)
			}
			if strings.Contains(out.String(), tt.absent) {
				t.Errorf("reportStatus() = %q, want it not to carry %q", out.String(), tt.absent)
			}
			if !strings.Contains(out.String(), tt.glyph+"  ") {
				t.Errorf("reportStatus() = %q, want it to carry the %q mark", out.String(), tt.glyph)
			}
		})
	}
}

// ///////////////////////////////////////////////
// removeProgram
// ///////////////////////////////////////////////

// TestRemoveProgram covers taking the installed binary out, including the
// case where the program being removed is the one running.
//
// The two answers are the two platform behaviours. Unix unlinks a running
// image and the process carries on. Windows refuses every delete of one,
// measured for both a plain delete and a POSIX-semantics delete, and allows
// a rename. The fallback is reached by refusing the delete rather than by
// asking which platform this is, so both branches run everywhere.
func TestRemoveProgram(t *testing.T) {
	t.Run("deletes a program nothing is running", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "stream-dvr"+exeSuffix())
		if err := os.WriteFile(target, []byte("not really a binary"), 0o700); err != nil {
			t.Fatalf("seeding the program: %v", err)
		}

		aside, err := removeProgram(target)
		if err != nil {
			t.Fatalf("removeProgram() err = %v, want nil", err)
		}
		if aside != "" {
			t.Errorf("removeProgram() = %q, want it deleted outright", aside)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("the program is still at %s, want it gone", target)
		}
	})

	t.Run("moves a program it cannot delete out of the install directory", func(t *testing.T) {
		const body = "not really a binary"
		target := filepath.Join(t.TempDir(), "stream-dvr"+exeSuffix())
		if err := os.WriteFile(target, []byte(body), 0o700); err != nil {
			t.Fatalf("seeding the program: %v", err)
		}

		// What Windows does to a running image.
		restore := removeFile
		removeFile = func(string) error { return os.ErrPermission }
		t.Cleanup(func() { removeFile = restore })

		aside, err := removeProgram(target)
		if err != nil {
			t.Fatalf("removeProgram() err = %v, want the fallback to carry it", err)
		}
		if aside == "" {
			t.Fatal("removeProgram() = \"\", want the path it moved to")
		}
		t.Cleanup(func() { os.RemoveAll(filepath.Dir(aside)) })

		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("the program is still at %s, want the install directory clean", target)
		}
		// Moved, not truncated. A file left half there would be worse than
		// one left whole.
		moved, err := os.ReadFile(aside)
		if err != nil {
			t.Fatalf("reading the moved program: %v", err)
		}
		if string(moved) != body {
			t.Errorf("moved contents = %q, want %q", moved, body)
		}
		if filepath.Dir(aside) == filepath.Dir(target) {
			t.Errorf("moved to %s, want it out of the install directory", aside)
		}
	})

	t.Run("reports a program it can neither delete nor move", func(t *testing.T) {
		// Both refused means the operator has to act, so the failure has to
		// name the file rather than pass silently.
		restore := removeFile
		removeFile = func(string) error { return os.ErrPermission }
		t.Cleanup(func() { removeFile = restore })

		target := filepath.Join(t.TempDir(), "absent"+exeSuffix())

		aside, err := removeProgram(target)
		if err == nil {
			t.Fatal("removeProgram() err = nil for a file it could not touch, want an error")
		}
		if aside != "" {
			t.Errorf("removeProgram() = %q, want no path when it failed", aside)
		}
		if !strings.Contains(err.Error(), target) {
			t.Errorf("err = %v, want it to name %s", err, target)
		}
	})

	t.Run("leaves no empty directory behind when the move fails", func(t *testing.T) {
		// The fallback makes a directory before it knows the move works.
		// One left per failed uninstall would accumulate in temp forever.
		restore := removeFile
		removeFile = func(string) error { return os.ErrPermission }
		t.Cleanup(func() { removeFile = restore })

		before, err := filepath.Glob(filepath.Join(os.TempDir(), "stream-dvr-uninstall-*"))
		if err != nil {
			t.Fatalf("listing temp: %v", err)
		}

		if _, err := removeProgram(filepath.Join(t.TempDir(), "absent"+exeSuffix())); err == nil {
			t.Fatal("removeProgram() err = nil, want a failure to drive this case")
		}

		after, err := filepath.Glob(filepath.Join(os.TempDir(), "stream-dvr-uninstall-*"))
		if err != nil {
			t.Fatalf("listing temp: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("temp holds %d uninstall directories, want the %d it started with",
				len(after), len(before))
		}
	})
}

// ///////////////////////////////////////////////
// Naming
// ///////////////////////////////////////////////

func TestServiceName_MatchesTheBinary(t *testing.T) {
	// The registration is found by name with the platform's own tools, so it
	// must be the name the operator already knows.
	if serviceName != paths.BinaryName {
		t.Errorf("serviceName = %q, want %q", serviceName, paths.BinaryName)
	}
	if serviceDescription == "" {
		t.Error("serviceDescription is empty, want text the platform can show")
	}
}

// ///////////////////////////////////////////////
// installBinary
// ///////////////////////////////////////////////

// TestInstallBinary_PutsTheBinaryWhereOnlyTheOwnerCanWriteIt covers the copy
// that decides what an autostart entry runs at every boot.
//
// The recorded path is the security boundary: whatever stands there runs as
// the operator forever. Registering the binary where it was downloaded would
// record a directory whose write access nobody chose.
func TestInstallBinary_PutsTheBinaryWhereOnlyTheOwnerCanWriteIt(t *testing.T) {
	const body = "not really a binary"

	t.Run("copies into a directory that does not exist yet", func(t *testing.T) {
		running := filepath.Join(t.TempDir(), "downloaded"+exeSuffix())
		if err := os.WriteFile(running, []byte(body), 0o700); err != nil {
			t.Fatalf("writing the downloaded binary: %v", err)
		}
		target := filepath.Join(t.TempDir(), "Programs", "stream-dvr"+exeSuffix())

		got, copied, err := installBinary(running, target)
		if err != nil {
			t.Fatalf("installBinary() err = %v, want nil", err)
		}
		if !copied {
			t.Error("copied = false, want true for a target that was not there")
		}
		if got != target {
			t.Errorf("path = %q, want the install target %q", got, target)
		}
		landed, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("reading the installed binary: %v", err)
		}
		if string(landed) != body {
			t.Errorf("installed contents = %q, want %q", landed, body)
		}
	})

	t.Run("reinstalling over itself is not an error", func(t *testing.T) {
		// install run twice, or run from the installed copy, must not fail
		// and must not report a copy it did not make.
		running := filepath.Join(t.TempDir(), "stream-dvr"+exeSuffix())
		if err := os.WriteFile(running, []byte(body), 0o700); err != nil {
			t.Fatalf("writing the installed binary: %v", err)
		}

		got, copied, err := installBinary(running, running)
		if err != nil {
			t.Fatalf("installBinary() err = %v, want nil", err)
		}
		if copied {
			t.Error("copied = true, want false when the binary is already in place")
		}
		if got != running {
			t.Errorf("path = %q, want %q", got, running)
		}
	})

	t.Run("a missing source is named", func(t *testing.T) {
		absent := filepath.Join(t.TempDir(), "gone"+exeSuffix())
		if _, _, err := installBinary(absent, filepath.Join(t.TempDir(), "x")); err == nil {
			t.Error("err = nil, want the missing source reported")
		}
	})
}

// exeSuffix returns the host's executable suffix, so the fixtures look like
// what the platform actually installs.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
