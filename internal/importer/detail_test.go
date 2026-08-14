package importer

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Media files
// ///////////////////////////////////////////////

func TestIsMedia_AcceptsEveryContainerTheProjectWrites(t *testing.T) {
	// config.Containers is the setting a recording is remuxed into. A file
	// in a container this walks past is invisible to the library forever,
	// so the two lists have to be one.
	for _, container := range config.Containers {
		if !isMedia("channel/2026/a." + container) {
			t.Errorf("isMedia() passed over %q, which capture.container accepts", container)
		}
		if !isMedia("channel/2026/a." + strings.ToUpper(container)) {
			t.Errorf("isMedia() is case sensitive, and a filesystem is not: %q", container)
		}
	}
}

func TestIsMedia_IgnoresWhatIsNotARecording(t *testing.T) {
	for _, path := range []string{
		"channel/2026/notes.txt",
		"channel/2026/thumb.png",
		"channel/2026/a.mkv.json",
		"channel/2026/noextension",
		"channel/2026/.mkv",
	} {
		if isMedia(path) {
			t.Errorf("isMedia(%q) = true, want false", path)
		}
	}
}

func TestReserved_SkipsWhatTheLibraryKeepsForItself(t *testing.T) {
	// paths.ReservedDirNames is the same list the name renderer refuses to
	// file a recording into. What an import walks past and what a rendered
	// name may never become are one definition.
	for _, name := range paths.ReservedDirNames {
		if !reserved(name) {
			t.Errorf("reserved(%q) = false, want the library's own directory skipped", name)
		}
		if !reserved(name + "/nested/deeper") {
			t.Errorf("reserved(%q) did not skip a path inside it", name)
		}
		if !reserved(strings.ToUpper(name)) {
			t.Errorf("reserved(%q) is case sensitive, and Windows is not", name)
		}
	}

	for _, ordinary := range []string{"atrioc", "atrioc/2026", "incomingband", ".dvrarchive"} {
		if reserved(ordinary) {
			t.Errorf("reserved(%q) = true, want an ordinary channel directory walked", ordinary)
		}
	}
}

// ///////////////////////////////////////////////
// Reconciling a sidecar against its file
// ///////////////////////////////////////////////

func TestDisagreements_ReportsOnlyWhatTheFileContradicts(t *testing.T) {
	// Zero is how this project spells nobody measured it. Reporting it as a
	// contradiction would put a warning beside every recording an older
	// build wrote, and a warning on everything is a warning on nothing.
	tests := []struct {
		name     string
		sidecar  organize.Sidecar
		measured measurement
		want     int
		why      string
	}{
		{
			name:     "a sidecar that agrees",
			sidecar:  organize.Sidecar{Bytes: 1024, MediaDurationMS: 3_600_000},
			measured: measurement{bytes: 1024, media: time.Hour},
			want:     0,
			why:      "nothing to report",
		},
		{
			name:     "a sidecar that measured nothing",
			sidecar:  organize.Sidecar{Bytes: 0, MediaDurationMS: 0},
			measured: measurement{bytes: 1024, media: time.Hour},
			want:     0,
			why:      "zero is an absent measurement, not a wrong one",
		},
		{
			name:     "a size that disagrees",
			sidecar:  organize.Sidecar{Bytes: 99, MediaDurationMS: 3_600_000},
			measured: measurement{bytes: 1024, media: time.Hour},
			want:     1,
			why:      "the file is the authority on its own size",
		},
		{
			name:     "a length that disagrees",
			sidecar:  organize.Sidecar{Bytes: 1024, MediaDurationMS: 60_000},
			measured: measurement{bytes: 1024, media: time.Hour},
			want:     1,
			why:      "the media is the authority on its own length",
		},
		{
			name:     "both disagreeing",
			sidecar:  organize.Sidecar{Bytes: 99, MediaDurationMS: 60_000},
			measured: measurement{bytes: 1024, media: time.Hour},
			want:     2,
			why:      "each is its own fact",
		},
		{
			name:     "a length inside the slack",
			sidecar:  organize.Sidecar{Bytes: 1024, MediaDurationMS: 3_600_500},
			measured: measurement{bytes: 1024, media: time.Hour},
			want:     0,
			why:      "a remux moves container timing, and half a second is the same recording",
		},
		{
			name:     "a length nothing could measure",
			sidecar:  organize.Sidecar{Bytes: 1024, MediaDurationMS: 3_600_000},
			measured: measurement{bytes: 1024, media: 0},
			want:     0,
			why:      "an unmeasured file contradicts nothing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := disagreements(tt.sidecar, tt.measured)

			if len(got) != tt.want {
				t.Errorf("disagreements() = %v, want %d of them, because %s", got, tt.want, tt.why)
			}
			if !slices.IsSorted(got) {
				t.Errorf("disagreements() = %v, want them sorted", got)
			}
		})
	}
}

func TestClaimedOrMeasured_PrefersTheRecordedDuration(t *testing.T) {
	// The sidecar's duration is a clock around the capture and the
	// measurement is the media. A capture that dropped a stretch ran longer
	// than the footage it produced, so where the sidecar has an answer it is
	// the better one.
	tests := []struct {
		name      string
		claimedMS int64
		measured  time.Duration
		want      time.Duration
	}{
		{name: "the sidecar knows", claimedMS: 7_200_000, measured: time.Hour, want: 2 * time.Hour},
		{name: "the sidecar does not", claimedMS: 0, measured: time.Hour, want: time.Hour},
		{name: "neither does", claimedMS: 0, measured: 0, want: 0},
		{
			name:      "a negative claim is no claim",
			claimedMS: -1,
			measured:  time.Hour,
			want:      time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claimedOrMeasured(tt.claimedMS, tt.measured); got != tt.want {
				t.Errorf("claimedOrMeasured(%d, %s) = %s, want %s",
					tt.claimedMS, tt.measured, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Reporting
// ///////////////////////////////////////////////

func TestDispositionFor_TreatsAClaimedPathAsWorkAlreadyDone(t *testing.T) {
	// UNIQUE(path) is what stops two rows naming one file. The loser has to
	// read as a skip: reporting a failure would have an operator chasing an
	// error that means the import succeeded.
	skip := dispositionFor("a/b.mkv", store.ErrDuplicatePath)
	if skip.Disposition != Skipped {
		t.Errorf("disposition = %q, want %q", skip.Disposition, Skipped)
	}
	if skip.Reason == "" {
		t.Error("a skip with no reason tells the operator nothing about why")
	}

	// Anything else is a genuine failure and must not be quietly counted as
	// done.
	refused := dispositionFor("a/b.mkv", errors.New("the disk went away"))
	if refused.Disposition != Refused {
		t.Errorf("disposition = %q, want %q", refused.Disposition, Refused)
	}
	if !strings.Contains(refused.Reason, "disk went away") {
		t.Errorf("reason = %q, want the cause carried", refused.Reason)
	}
}

func TestLossyNote_NamesEveryReadingWorthQuestioning(t *testing.T) {
	tests := []struct {
		name  string
		match naming.Match
		want  []string
	}{
		{name: "a clean reading", match: naming.Match{}, want: nil},
		{
			name:  "a sanitized field",
			match: naming.Match{Lossy: []string{"title"}},
			want:  []string{"title"},
		},
		{
			name:  "a deduplication suffix",
			match: naming.Match{Duplicate: 3},
			want:  []string{"deduplication", "(3)"},
		},
		{
			name:  "both at once",
			match: naming.Match{Lossy: []string{"title"}, Duplicate: 2},
			want:  []string{"title", "deduplication"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lossyNote(tt.match)

			if len(tt.want) == 0 {
				if got != "" {
					t.Errorf("lossyNote() = %q, want nothing to report", got)
				}
				return
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("lossyNote() = %q, want it to mention %q", got, want)
				}
			}
		})
	}
}

func TestReport_CountsWhatItDid(t *testing.T) {
	report := Report{Files: []File{
		{Disposition: Restored},
		{Disposition: Restored},
		{Disposition: Inferred},
		{Disposition: Skipped},
		{Disposition: Refused},
	}}

	for disposition, want := range map[Disposition]int{
		Restored: 2, Inferred: 1, Skipped: 1, Refused: 1,
	} {
		if got := report.Count(disposition); got != want {
			t.Errorf("Count(%q) = %d, want %d", disposition, got, want)
		}
	}
	// Imported is the two tiers that made a row, and nothing else. Counting
	// a skip or a refusal here would report work that did not happen.
	if got := report.Imported(); got != 3 {
		t.Errorf("Imported() = %d, want 3", got)
	}
}

func TestJoin_DropsTheEmptyHalf(t *testing.T) {
	tests := []struct {
		first, second, want string
	}{
		{first: "", second: "", want: ""},
		{first: "a", second: "", want: "a"},
		{first: "", second: "b", want: "b"},
		{first: "a", second: "b", want: "a; b"},
	}

	for _, tt := range tests {
		if got := join(tt.first, tt.second); got != tt.want {
			t.Errorf("join(%q, %q) = %q, want %q", tt.first, tt.second, got, tt.want)
		}
	}
}
