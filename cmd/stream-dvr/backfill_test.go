package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/backfill"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/daemon"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/store"
)

// backfillConfig writes a config over a real library and returns its path.
func backfillConfig(t *testing.T) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "recordings")
	if _, err := library.Create(root, "test"); err != nil {
		t.Fatalf("creating a library: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[library]\nroot = " + tomlString(root) + "\n\n" +
		"[[channels]]\nplatform = \"twitch\"\nname = \"examplechannel\"\n" +
		"enabled = true\nbackfill = true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing a config: %v", err)
	}
	return path
}

// heldLibrary claims the library a config names, the way a running recorder
// does, and returns the store still holding it.
func heldLibrary(t *testing.T, configPath string) *store.Store {
	t.Helper()

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want nil", err)
	}
	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		t.Fatalf("opening the library: %v", err)
	}
	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open() err = %v, want nil", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.StartSession(time.Now(), daemon.StaleAfter); err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	return db
}

func TestBackfillRange(t *testing.T) {
	// A pass downloads hours of video from somebody else's service, so the
	// range is the operator's to name. The spellings are the config's, so
	// "3d" means one thing across the whole tool rather than being refused
	// here and accepted there.
	tests := []struct {
		name    string
		text    string
		want    time.Duration
		wantErr bool
	}{
		{name: "hours", text: "24h", want: 24 * time.Hour},
		{name: "days", text: "3d", want: 72 * time.Hour},
		{name: "minutes", text: "90m", want: 90 * time.Minute},
		{name: "padded", text: "  2h  ", want: 2 * time.Hour},
		{name: "nothing at all", text: "", wantErr: true},
		{name: "only spaces", text: "   ", wantErr: true},
		{name: "zero", text: "0h", wantErr: true},
		{name: "negative", text: "-1h", wantErr: true},
		{name: "not a duration", text: "soon", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := backfillRange(tt.text)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("backfillRange(%q) err = nil, want a refusal", tt.text)
				}
				return
			}
			if err != nil {
				t.Fatalf("backfillRange(%q) err = %v, want nil", tt.text, err)
			}
			if got != tt.want {
				t.Errorf("backfillRange(%q) = %s, want %s", tt.text, got, tt.want)
			}
		})
	}
}

func TestBackfillRange_NamesTheFlagWhenNoneWasGiven(t *testing.T) {
	// The whole point of refusing rather than defaulting is that the
	// operator picks the range, so the refusal has to say how.
	_, err := backfillRange("")
	if err == nil {
		t.Fatal("backfillRange() err = nil, want a refusal")
	}
	for _, want := range []string{"--since", "24h"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

func TestRunBackfill_PlansAChannelTheRecorderHasNeverSeen(t *testing.T) {
	// Only the recorder's poller ever created a channel row, so a library
	// the daemon has never run against knew no channel at all. A pass on a
	// fresh install reported that no channel had backfill turned on, while
	// the config it had just read plainly said one did.
	configPath := backfillConfig(t)

	var out bytes.Buffer
	if err := runBackfill(context.Background(), &out, configPath, 24*time.Hour, true); err != nil {
		t.Fatalf("runBackfill() err = %v, want nil", err)
	}
	if strings.Contains(out.String(), "no channel has backfill") {
		t.Errorf("the channel in the config was not considered\ngot:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "examplechannel") {
		t.Errorf("output did not name the channel it planned\ngot:\n%s", out.String())
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want nil", err)
	}
	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		t.Fatalf("opening the library: %v", err)
	}
	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open() err = %v, want nil", err)
	}
	defer db.Close()

	if _, err := db.Channel("twitch", "examplechannel"); err != nil {
		t.Errorf("db.Channel() err = %v, want the channel recorded so a pass can plan it", err)
	}
}

func TestRunBackfill_RefusesWhileARecorderHoldsTheLibrary(t *testing.T) {
	// A second writer would race the daemon's sweep. The dry run refuses
	// too, because a plan computed while the daemon acts is a plan about a
	// library that is already moving.
	for _, dryRun := range []bool{true, false} {
		name := "a pass"
		if dryRun {
			name = "a dry run"
		}
		t.Run(name, func(t *testing.T) {
			configPath := backfillConfig(t)
			heldLibrary(t, configPath)

			var out bytes.Buffer
			err := runBackfill(context.Background(), &out, configPath, 24*time.Hour, dryRun)
			if err == nil {
				t.Fatal("runBackfill() err = nil while a recorder holds the library, want a refusal")
			}
			if !strings.Contains(err.Error(), "recorder holds this library") {
				t.Errorf("runBackfill() err = %v, want it to name the running recorder", err)
			}
		})
	}
}

func TestRunBackfill_ReleasesTheLibraryWhenThePassEnds(t *testing.T) {
	// The claim is what stops a recorder starting mid-pass. One left open
	// blocks every later run until it goes stale, and nothing would say why.
	configPath := backfillConfig(t)

	var out bytes.Buffer
	if err := runBackfill(context.Background(), &out, configPath, 24*time.Hour, false); err != nil {
		t.Fatalf("runBackfill() err = %v, want nil", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want nil", err)
	}
	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		t.Fatalf("opening the library: %v", err)
	}
	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open() err = %v, want nil", err)
	}
	defer db.Close()

	running, err := db.RecorderRunning(time.Now(), daemon.StaleAfter)
	if err != nil {
		t.Fatalf("RecorderRunning() err = %v, want nil", err)
	}
	if running {
		t.Error("the pass left its claim open, so a recorder cannot start until it goes stale")
	}
}

func TestRunBackfill_TakesTheClaimSoARecorderCannotStart(t *testing.T) {
	// Reading whether a recorder runs and then acting leaves a window where
	// one starts in between. The claim closes it, and the proof is that a
	// recorder cannot take one while a pass holds it.
	configPath := backfillConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() err = %v, want nil", err)
	}
	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		t.Fatalf("opening the library: %v", err)
	}

	held, err := store.Open(lib.DatabasePath())
	if err != nil {
		t.Fatalf("store.Open() err = %v, want nil", err)
	}
	defer held.Close()
	session, err := held.StartSession(time.Now(), daemon.StaleAfter)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	// What a recorder starting mid-pass would hit.
	if _, err := held.StartSession(time.Now(), daemon.StaleAfter); err == nil {
		t.Error("a second claim succeeded while the first was open, so the claim guards nothing")
	}
	if err := held.StopSession(session.ID, time.Now()); err != nil {
		t.Fatalf("StopSession() err = %v, want nil", err)
	}
}

func TestRunBackfill_ReportsAConfigItCannotRead(t *testing.T) {
	var out bytes.Buffer
	if err := runBackfill(context.Background(), &out,
		filepath.Join(t.TempDir(), "absent.toml"), 24*time.Hour, true); err == nil {
		t.Error("runBackfill() err = nil for an unreadable config, want an error")
	}
}

func TestIncomingBytes(t *testing.T) {
	// A fetch writes here, and those bytes are invisible to the size cap
	// until the file is claimed. Leaving them out lets a pass fill the disk
	// the budget exists to guard.
	t.Run("a directory that does not exist yet", func(t *testing.T) {
		got, err := incomingBytes(filepath.Join(t.TempDir(), "incoming"))
		if err != nil {
			t.Fatalf("incomingBytes() err = %v, want nil", err)
		}
		if got != 0 {
			t.Errorf("incomingBytes() = %d, want 0 for a library nothing has written to", got)
		}
	})

	t.Run("files and a subdirectory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "one.ts"), make([]byte, 512), 0o600); err != nil {
			t.Fatalf("seeding a file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "two.ts"), make([]byte, 256), 0o600); err != nil {
			t.Fatalf("seeding a file: %v", err)
		}
		if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
			t.Fatalf("seeding a directory: %v", err)
		}

		got, err := incomingBytes(dir)
		if err != nil {
			t.Fatalf("incomingBytes() err = %v, want nil", err)
		}
		if got != 768 {
			t.Errorf("incomingBytes() = %d, want 768", got)
		}
	})
}

func TestRunPass_OutcomesCarryAnEventKindTheNotifierKnows(t *testing.T) {
	// The report closure converts one string type into the other, and the
	// compiler accepts that whatever the two values are. A value edited on
	// one side alone would deliver a kind nothing recognises, and the
	// operator would simply stop hearing about recovered broadcasts.
	tests := []struct {
		name    string
		outcome string
		want    daemon.EventKind
	}{
		{name: "recovered", outcome: backfill.OutcomeRecovered, want: daemon.EventRecovered},
		{name: "gap filled", outcome: backfill.OutcomeGapFilled, want: daemon.EventGapFilled},
		{name: "gave up", outcome: backfill.OutcomeGaveUp, want: daemon.EventFetchGaveUp},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := daemon.EventKind(test.outcome); got != test.want {
				t.Errorf("daemon.EventKind(%q) = %q, want %q", test.outcome, got, test.want)
			}
		})
	}
}
