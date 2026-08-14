package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/retention"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Fakes
// ///////////////////////////////////////////////

// fakePurge serves a scripted ranking and records what was trashed.
type fakePurge struct {
	candidates []retention.Candidate

	trashed    []int64
	rankCalls  int
	rankErr    error
	trashErrs  map[int64]error
	trashedErr error
}

func (f *fakePurge) Candidates() ([]retention.Candidate, error) {
	f.rankCalls++
	if f.rankErr != nil {
		return nil, f.rankErr
	}
	return f.candidates, nil
}

func (f *fakePurge) Trash(recordingID int64) error {
	if err, ok := f.trashErrs[recordingID]; ok {
		return err
	}
	if f.trashedErr != nil {
		return f.trashedErr
	}

	f.trashed = append(f.trashed, recordingID)
	f.candidates = removeCandidate(f.candidates, recordingID)
	return nil
}

// ///////////////////////////////////////////////
// Harness
// ///////////////////////////////////////////////

// removeCandidate drops a trashed recording from the ranking, which is what
// the real one does: a trashed row is no longer a state retention offers.
func removeCandidate(candidates []retention.Candidate, id int64) []retention.Candidate {
	kept := make([]retention.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Recording.ID != id {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// candidate builds one ranked row.
func candidate(id int64, path string, bytes int64, why ...string) retention.Candidate {
	reasons := make([]retention.Reason, 0, len(why))
	for _, reason := range why {
		reasons = append(reasons, retention.Reason{Why: reason})
	}

	return retention.Candidate{
		Recording: store.Recording{
			ID:        id,
			Path:      path,
			Bytes:     bytes,
			State:     store.StateComplete,
			StartedAt: fixedNow.Add(-30 * 24 * time.Hour),
		},
		Reasons: reasons,
	}
}

// rankedPurge returns a fake holding two candidates.
func rankedPurge() *fakePurge {
	return &fakePurge{candidates: []retention.Candidate{
		candidate(1, "ExampleChannel/2026/first.mkv", 3<<30, "already watched", "4 weeks old"),
		candidate(2, "ExampleChannel/2026/second.mkv", 5<<30, "4 weeks old"),
	}}
}

// openedPurge returns a model with the purge pane open over two candidates.
func openedPurge(t *testing.T, purge *fakePurge) *Model {
	t.Helper()

	model := newOptionsModel(t, Options{Library: library(), Purge: purge})
	press(t, model, "x")
	return model
}

// ///////////////////////////////////////////////
// Opening
// ///////////////////////////////////////////////

func TestModel_PurgeOpensOnAFreshRanking(t *testing.T) {
	// The ranking is a snapshot of a library the recorder is still writing
	// to. A cached one would offer rows that have since been finalized,
	// pinned, or already purged.
	purge := rankedPurge()
	model := openedPurge(t, purge)

	if model.pane != panePurge {
		t.Errorf("pane = %d after x, want the purge pane", model.pane)
	}
	if purge.rankCalls != 1 {
		t.Errorf("Candidates() called %d times, want 1", purge.rankCalls)
	}

	view := render(t, model)
	for _, want := range []string{"first.mkv", "second.mkv", "already watched", "4 weeks old"} {
		if !strings.Contains(view, want) {
			t.Errorf("the purge pane does not show %q:\n%s", want, view)
		}
	}
}

func TestModel_PurgeWithoutAPurgerSaysSo(t *testing.T) {
	model := newModel(t, library(), nil)

	press(t, model, "x")

	if model.pane == panePurge {
		t.Error("the purge pane opened with nothing behind it")
	}
	if model.Err() == nil || !strings.Contains(model.Err().Error(), "cannot be purged") {
		t.Errorf("Err() = %v, want it to say the library cannot be purged from here", model.Err())
	}
}

func TestModel_PurgeReportsARankingFailure(t *testing.T) {
	purge := &fakePurge{rankErr: errors.New("database is locked")}
	model := openedPurge(t, purge)

	if !strings.Contains(render(t, model), "database is locked") {
		t.Error("the purge pane does not say why it has no ranking")
	}
}

func TestModel_PurgeSaysWhenNothingMayGo(t *testing.T) {
	model := openedPurge(t, &fakePurge{})

	if !strings.Contains(render(t, model), "nothing may be purged") {
		t.Errorf("an empty ranking does not say so:\n%s", render(t, model))
	}
}

// ///////////////////////////////////////////////
// Selecting
// ///////////////////////////////////////////////

func TestModel_PurgeSelectionTotalsWhatWouldMove(t *testing.T) {
	model := openedPurge(t, rankedPurge())

	press(t, model, " ")
	pressNamed(t, model, tea.KeyDown)
	press(t, model, " ")

	view := render(t, model)
	if !strings.Contains(view, "2 recordings selected") {
		t.Errorf("the summary does not count the selection:\n%s", view)
	}
	if !strings.Contains(view, "8GiB") {
		t.Errorf("the summary does not total the selection:\n%s", view)
	}
}

func TestModel_PurgeSelectionToggles(t *testing.T) {
	model := openedPurge(t, rankedPurge())

	press(t, model, " ")
	press(t, model, " ")

	if len(model.selected) != 0 {
		t.Errorf("%d recordings stay selected after two presses, want 0", len(model.selected))
	}
	if !strings.Contains(render(t, model), "nothing selected") {
		t.Error("the summary does not report an empty selection")
	}
}

// ///////////////////////////////////////////////
// Confirming
// ///////////////////////////////////////////////

func TestModel_PurgeNeverActsOnOneKeyPress(t *testing.T) {
	// This is the only destructive path in the application. enter opens a
	// prompt; it does not move anything.
	purge := rankedPurge()
	model := openedPurge(t, purge)
	press(t, model, " ")

	pressNamed(t, model, tea.KeyEnter)

	if len(purge.trashed) != 0 {
		t.Errorf("enter moved %d recordings before any confirmation", len(purge.trashed))
	}
	if !model.confirming {
		t.Fatal("enter did not open the confirmation")
	}

	view := render(t, model)
	if !strings.Contains(view, "move 1 recording") {
		t.Errorf("the prompt does not say what it would move:\n%s", view)
	}
	if !strings.Contains(view, "to the trash") {
		t.Errorf("the prompt does not say the recording goes to the trash:\n%s", view)
	}
}

func TestModel_PurgeWithNothingSelectedDoesNotPrompt(t *testing.T) {
	model := openedPurge(t, rankedPurge())

	pressNamed(t, model, tea.KeyEnter)

	if model.confirming {
		t.Error("enter opened a confirmation over an empty selection")
	}
}

func TestModel_PurgeConfirmationIsCancellable(t *testing.T) {
	purge := rankedPurge()
	model := openedPurge(t, purge)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyEnter)

	pressNamed(t, model, tea.KeyEsc)

	if model.confirming {
		t.Fatal("esc left the confirmation standing")
	}
	if len(purge.trashed) != 0 {
		t.Errorf("esc moved %d recordings", len(purge.trashed))
	}
	if model.pane != panePurge {
		t.Error("esc left the pane instead of cancelling the confirmation")
	}
	if len(model.selected) != 1 {
		t.Error("cancelling the confirmation dropped the selection")
	}
}

func TestModel_PurgeConfirmationSwallowsEveryOtherKey(t *testing.T) {
	// A prompt naming a count must not be able to describe a different set
	// by the time it is answered.
	purge := rankedPurge()
	model := openedPurge(t, purge)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyEnter)

	pressNamed(t, model, tea.KeyDown)
	press(t, model, " ")

	if model.candidate != 0 {
		t.Errorf("candidate = %d under a confirmation, want 0", model.candidate)
	}
	if len(model.selected) != 1 {
		t.Errorf("%d recordings selected under a confirmation, want 1", len(model.selected))
	}
}

// ///////////////////////////////////////////////
// Purging
// ///////////////////////////////////////////////

func TestModel_PurgeMovesTheWholeSelectionOnce(t *testing.T) {
	purge := rankedPurge()
	model := openedPurge(t, purge)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyDown)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyEnter)

	pressNamed(t, model, tea.KeyEnter)

	if len(purge.trashed) != 2 {
		t.Fatalf("Trash called for %v, want both recordings", purge.trashed)
	}
	if model.confirming {
		t.Error("the confirmation still stands after the purge ran")
	}
	if !strings.Contains(render(t, model), "2 recordings moved to the trash") {
		t.Errorf("the pane does not report what moved:\n%s", render(t, model))
	}
}

func TestModel_PurgeRedrawsFromTheLibraryAfterwards(t *testing.T) {
	// The purged rows are gone from the ranking and marked trashed on the
	// calendar. A screen still offering them is what makes an operator
	// purge the same recording twice.
	purge := rankedPurge()
	model := openedPurge(t, purge)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyEnter)
	pressNamed(t, model, tea.KeyEnter)

	if len(model.candidates) != 1 {
		t.Errorf("%d candidates remain on screen, want the purged one gone", len(model.candidates))
	}
	if purge.rankCalls != 2 {
		t.Errorf("Candidates() called %d times, want a reload after the purge", purge.rankCalls)
	}
	if len(model.selected) != 0 {
		t.Errorf("%d recordings stay selected over a new ranking", len(model.selected))
	}
}

func TestModel_PurgeReportsWhatFailedAndStillMovesTheRest(t *testing.T) {
	// A purge is the answer to a full library. Stopping at the first row
	// the organizer happens to hold would free almost nothing.
	purge := rankedPurge()
	purge.trashErrs = map[int64]error{1: errors.New("recording 1: busy")}
	model := openedPurge(t, purge)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyDown)
	press(t, model, " ")
	pressNamed(t, model, tea.KeyEnter)

	pressNamed(t, model, tea.KeyEnter)

	if len(purge.trashed) != 1 || purge.trashed[0] != 2 {
		t.Errorf("Trash succeeded for %v, want the one that was not busy", purge.trashed)
	}
	// The reload that always follows a purge must not clear the report.
	view := render(t, model)
	if !strings.Contains(view, "busy") {
		t.Errorf("the pane does not say what failed:\n%s", view)
	}
	if !strings.Contains(view, "1 recording moved to the trash") {
		t.Errorf("the pane does not say what still moved:\n%s", view)
	}
}

// ///////////////////////////////////////////////
// Leaving
// ///////////////////////////////////////////////

func TestModel_PurgeEscReturnsToTheCalendar(t *testing.T) {
	model := openedPurge(t, rankedPurge())

	pressNamed(t, model, tea.KeyEsc)

	if model.pane != paneCalendar {
		t.Errorf("pane = %d after esc, want the calendar", model.pane)
	}
	if model.Quit() {
		t.Error("esc quit the application instead of leaving the pane")
	}
}

func TestModel_PurgeQuits(t *testing.T) {
	model := openedPurge(t, rankedPurge())

	press(t, model, "q")

	if !model.Quit() {
		t.Error("q did not quit from the purge pane")
	}
}

func TestReasonsFor_SaysWhyARecordingScored(t *testing.T) {
	cases := []struct {
		name      string
		candidate retention.Candidate
		want      string
	}{
		{
			name:      "several reasons",
			candidate: candidate(1, "a.mkv", 1, "already watched", "4 weeks old"),
			want:      "already watched, 4 weeks old",
		},
		{
			name:      "one reason",
			candidate: candidate(1, "a.mkv", 1, "already watched"),
			want:      "already watched",
		},
		{
			name:      "no reason at all",
			candidate: candidate(1, "a.mkv", 1),
			want:      "no reason scored",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A ranking the operator cannot interrogate is one they have to
			// trust blindly at the moment it proposes deleting a broadcast.
			if got := reasonsFor(tc.candidate); got != tc.want {
				t.Errorf("reasonsFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPurge_RefusesASecondEnterBeforeTheFirstAnswers(t *testing.T) {
	// The confirmation only clears when the answer comes back, so key
	// autorepeat on enter turns a gate that costs two deliberate presses
	// into one trash attempt per repeat, against the same selection, on
	// the only destructive path in the application.
	model := everyPane(t)
	press(t, model, "x")
	_ = model.View()

	if len(model.candidates) == 0 {
		t.Skip("the fixture library offers nothing to purge")
	}

	press(t, model, "space")
	press(t, model, "enter")
	if !model.confirming {
		t.Fatal("the confirmation did not open")
	}

	first := model.handleKey("enter")
	if first == nil {
		t.Fatal("the first enter issued no purge")
	}
	for range 4 {
		if again := model.handleKey("enter"); again != nil {
			t.Error("a second enter issued another purge before the first answered")
		}
	}
}
