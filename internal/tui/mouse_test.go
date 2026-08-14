package tui

import (
	"testing"
)

func TestHandleClick_AClickIsTheKeyItNames(t *testing.T) {
	// The whole mouse model. No handler exists for anything that already has
	// a keyboard binding, so a chip and a click reach the same place.
	model := everyPane(t)
	_ = model.View()

	region, ok := regionFor(model, "x")
	if !ok {
		t.Skip("nothing on this screen dispatches x, so there is nothing to click")
	}

	drain(t, model, model.handleClick(region.Col, region.Row))

	if model.pane != panePurge {
		t.Errorf("clicking the purge chip left the pane at %d, want the purge", model.pane)
	}
}

func TestHandleClick_ADayCellSelectsAndOpensIt(t *testing.T) {
	// One click. The cell carries a day number and a state and nothing else,
	// so "just looking" tells an operator almost nothing and opening is what
	// they wanted.
	model := everyPane(t)
	_ = model.View()

	region, ok := regionForValue(model, "day", "2026-03-10")
	if !ok {
		t.Fatal("no calendar cell registered for 10 March")
	}

	drain(t, model, model.handleClick(region.Col, region.Row))

	if got := model.Cursor().Format("2006-01-02"); got != "2026-03-10" {
		t.Errorf("Cursor() = %s after clicking the 10th, want 2026-03-10", got)
	}
	if !model.dayOpen {
		t.Error("clicking a day did not open it")
	}
}

func TestHandleClick_TheModalsWhitespaceSwallowsAClick(t *testing.T) {
	// A backdrop click closes the modal, and its own whitespace does not.
	// Order is the mechanism: the full-screen region goes down first, the
	// modal's rectangle over it, and later regions win.
	model := everyPane(t)
	press(t, model, "enter")
	_ = model.View()

	inside := model.frame.Modal
	drain(t, model, model.handleClick(inside.X+inside.W/2, inside.Y+inside.H-2))
	if !model.dayOpen {
		t.Error("a click inside the modal closed it")
	}

	drain(t, model, model.handleClick(1, model.frame.Status.Y))
	if model.dayOpen {
		t.Error("a click on the backdrop did not close the modal")
	}
}

func TestHandleClick_NothingUnderThePointerDoesNothing(t *testing.T) {
	model := everyPane(t)
	_ = model.View()

	before := model.View().Content
	if cmd := model.handleClick(9999, 9999); cmd != nil {
		t.Error("a click off the screen produced a command")
	}
	if model.View().Content != before {
		t.Error("a click off the screen changed the screen")
	}
}

func TestHandleWheel_OverTheGridTurnsThePage(t *testing.T) {
	// The one gesture a calendar has that a list does not.
	model := everyPane(t)
	_ = model.View()

	inner := model.frame.Calendar.inner()
	drain(t, model, model.handleWheel(inner.X+2, inner.Y+2, false))

	if got := model.Month().Format("2006-01"); got != "2026-04" {
		t.Errorf("Month() = %s after a wheel down over the grid, want April", got)
	}

	drain(t, model, model.handleWheel(inner.X+2, inner.Y+2, true))
	if got := model.Month().Format("2006-01"); got != "2026-03" {
		t.Errorf("Month() = %s after a wheel up, want March again", got)
	}
}

func TestHandleWheel_OverTheQueueScrollsIt(t *testing.T) {
	model := everyPane(t)
	_ = model.View()

	before := model.queueAt
	drain(t, model, model.handleWheel(model.frame.Side.X+4, model.frame.Side.Y+4, true))

	if model.queueAt == before && before != 0 {
		t.Errorf("the wheel over the queue moved nothing from %d", before)
	}
	if got := model.Month().Format("2006-01"); got != "2026-03" {
		t.Errorf("the wheel over the queue turned the page to %s", got)
	}
}

func TestAtoi_RefusesAnythingButDigits(t *testing.T) {
	// A region's value is text from a click, so it is parsed rather than
	// trusted.
	tests := []struct {
		text string
		want int
	}{
		{text: "0", want: 0},
		{text: "12", want: 12},
		{text: "", want: 0},
		{text: "-1", want: 0},
		{text: "3a", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := atoi(tt.text); got != tt.want {
				t.Errorf("atoi(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

// regionFor returns the first registered region dispatching a key.
func regionFor(model *Model, key string) (region, bool) {
	for _, r := range model.hits.regions {
		if r.Key == key {
			return r, true
		}
	}
	return region{}, false
}

// regionForValue returns the registered region carrying a key and value.
func regionForValue(model *Model, key, value string) (region, bool) {
	for _, r := range model.hits.regions {
		if r.Key == key && r.Value == value {
			return r, true
		}
	}
	return region{}, false
}
