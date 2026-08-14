package tui

import (
	"strings"
	"testing"
)

func TestSideBody_IsNeverEmpty(t *testing.T) {
	// A clean month has an empty queue and a full by-week table, so the
	// panel never reads as broken for want of anything to recover.
	model := newModel(t, library(), nil)
	_ = model.View()

	body := stripANSI(strings.Join(model.sideBody(model.frame.Side.inner()), "\n"))

	for _, want := range []string{"nothing in this month needs fetching", "by week", "month"} {
		if !strings.Contains(body, want) {
			t.Errorf("a clean month's panel does not carry %q:\n%s", want, body)
		}
	}
}

func TestQueueLines_ListMissedAndPartlyCapturedDaysAlike(t *testing.T) {
	// Cell.Recoverable is the one rule the work list and the single-day
	// action share, so a day the list skips is a day nothing else offers to
	// fetch. Listing only missed days silently dropped every partial one.
	model := everyPane(t)
	_ = model.View()

	if len(model.queue) != 3 {
		t.Fatalf("the queue holds %d days, want the two missed and the one partial",
			len(model.queue))
	}

	body := stripANSI(strings.Join(model.sideBody(model.frame.Side.inner()), "\n"))
	for _, want := range []string{"Mon 2 Mar", "Tue 3 Mar", "Wed 4 Mar"} {
		if !strings.Contains(body, want) {
			t.Errorf("the queue does not list %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "partly captured") {
		t.Errorf("the queue drops the partly captured day's state:\n%s", body)
	}
}

func TestQueueLines_CountWhatIsLeftToFetch(t *testing.T) {
	model := everyPane(t)
	_ = model.View()

	body := stripANSI(strings.Join(model.sideBody(model.frame.Side.inner()), "\n"))

	if !strings.Contains(body, "3 days") {
		t.Errorf("the queue does not count its days:\n%s", body)
	}
	if !strings.Contains(body, "oldest first") {
		t.Errorf("the queue does not say which end it starts at:\n%s", body)
	}
}

func TestWeekLines_SumToTheMonth(t *testing.T) {
	// Row three of the table is week row three of the grid, so the two are
	// spatially linked. The figures have to add up to the month below them
	// or the link is decoration.
	model := everyPane(t)
	_ = model.View()

	rows := model.weekLines(model.frame.Side.inner().W)

	if want := len(model.grid.Weeks) + 2; len(rows) != want {
		t.Errorf("the by-week table has %d rows, want a heading, a header and %d weeks",
			len(rows), len(model.grid.Weeks))
	}
}

func TestMonthLines_SplitTheDaysByHowTheyEndedUp(t *testing.T) {
	model := everyPane(t)
	_ = model.View()

	lines := stripANSI(strings.Join(model.monthLines(70), "\n"))

	for _, want := range []string{"missed", "recording", "nothing was watching"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the month block does not carry %q:\n%s", want, lines)
		}
	}
}

func TestNarrowColumn_HoldsTheLegendBesideTheGrid(t *testing.T) {
	// Thirteen rows against the grid's thirteen, which is what makes the
	// single-panel arrangement fit at 80 columns.
	model := newModel(t, library(), nil)
	model.width, model.height = 80, 24
	_ = model.View()

	column := model.narrowColumn(model.frame.Side.W)

	if len(column) != gridRows {
		t.Errorf("the side column is %d rows, want %d beside the grid", len(column), gridRows)
	}
	if !strings.Contains(stripANSI(strings.Join(column, "\n")), "coverage") {
		t.Errorf("the side column drops the legend:\n%s", stripANSI(strings.Join(column, "\n")))
	}
}

func TestPadBlock_HoldsItsHeightBothWays(t *testing.T) {
	// What is under a block must not move when its content shortens, and a
	// block that outgrew its slot must not push it down.
	if got := padBlock([]string{"a"}, 4); len(got) != 4 {
		t.Errorf("padBlock() grew a short block to %d rows, want 4", len(got))
	}
	if got := padBlock([]string{"a", "b", "c"}, 2); len(got) != 2 {
		t.Errorf("padBlock() left a long block at %d rows, want 2", len(got))
	}
	if got := padBlock(nil, 0); len(got) != 0 {
		t.Errorf("padBlock() with no room drew %d rows", len(got))
	}
}
