package tui

import (
	"testing"
)

func TestEnterQueue_LandsOnTheNextThingWorthActingOn(t *testing.T) {
	// A day with nothing to recover is not in the queue, so moving focus
	// there from a settled day lands on the next entry rather than on
	// nothing. That is what the queue is for.
	model := everyPane(t)
	press(t, model, "t")

	drain(t, model, model.enterQueue())

	if len(model.queue) == 0 {
		t.Fatal("the fixture has nothing to recover, so this proved nothing")
	}
	if got := model.Cursor().Format("2006-01-02"); got != model.queue[model.queueAt].Date.Format("2006-01-02") {
		t.Errorf("the cursor is on %s and the caret on %s, want them together",
			got, model.queue[model.queueAt].Date.Format("2006-01-02"))
	}
}

func TestEnterQueue_AnEmptyQueueMovesNothing(t *testing.T) {
	model := newModel(t, library(), nil)
	before := model.Cursor()

	drain(t, model, model.enterQueue())

	if !model.Cursor().Equal(before) {
		t.Errorf("Cursor() = %s, want it left alone with nothing to recover", model.Cursor())
	}
	if model.queueAt != 0 {
		t.Errorf("queueAt = %d over an empty queue, want 0", model.queueAt)
	}
}

func TestMoveQueue_CarriesTheCalendarCursorWithIt(t *testing.T) {
	// The two are one selection shown twice. A queue that moved on its own
	// would leave the grid pointing at a day the modal would not open.
	model := everyPane(t)
	drain(t, model, model.enterQueue())
	model.queueAt = 0
	model.cursor = model.queue[0].Date

	drain(t, model, model.moveQueue(1))

	if model.queueAt != 1 {
		t.Fatalf("queueAt = %d after one step, want 1", model.queueAt)
	}
	if got := model.Cursor().Format("2006-01-02"); got != model.queue[1].Date.Format("2006-01-02") {
		t.Errorf("Cursor() = %s, want the queue's second entry", got)
	}
}

func TestMoveQueue_ClampsAtBothEnds(t *testing.T) {
	model := everyPane(t)
	drain(t, model, model.enterQueue())

	drain(t, model, model.moveQueue(-99))
	if model.queueAt != 0 {
		t.Errorf("queueAt = %d past the top, want 0", model.queueAt)
	}

	drain(t, model, model.moveQueue(99))
	if want := len(model.queue) - 1; model.queueAt != want {
		t.Errorf("queueAt = %d past the bottom, want %d", model.queueAt, want)
	}
}

func TestSelectDay_MovesTheCursorAndPointsTheQueue(t *testing.T) {
	// A click picks a day. The caret marks the selection in both panels at
	// once, so the queue follows without becoming a second cursor.
	model := everyPane(t)
	stamp := model.queue[len(model.queue)-1].Date.Format("2006-01-02")

	drain(t, model, model.selectDay(stamp))

	if got := model.Cursor().Format("2006-01-02"); got != stamp {
		t.Errorf("Cursor() = %s, want %s", got, stamp)
	}
	if got := model.queue[model.queueAt].Date.Format("2006-01-02"); got != stamp {
		t.Errorf("the queue's caret is on %s, want %s", got, stamp)
	}
}

func TestSelectDay_RefusesAnythingButADateTheGridHolds(t *testing.T) {
	// The date arrives as text from a click, so it is parsed rather than
	// trusted.
	model := everyPane(t)
	before := model.Cursor()

	for _, stamp := range []string{"", "not-a-date", "2020-01-01", "2026-03-99"} {
		t.Run(stamp, func(t *testing.T) {
			drain(t, model, model.selectDay(stamp))

			if !model.Cursor().Equal(before) {
				t.Errorf("Cursor() = %s after %q, want it left alone", model.Cursor(), stamp)
			}
		})
	}
}

func TestSelectChannel_IgnoresANumberNothingIsUnder(t *testing.T) {
	model := newModel(t, library(), nil)

	for _, index := range []int{-1, 1, 9} {
		if cmd := model.selectChannel(index); cmd != nil {
			t.Errorf("selectChannel(%d) reloaded for a channel that is not there", index)
		}
	}
}
