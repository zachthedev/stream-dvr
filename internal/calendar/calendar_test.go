package calendar

import (
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// day builds a coverage entry for a day of August 2026.
func day(dayOfMonth int, state store.Coverage) store.Day {
	return store.Day{
		Date:  time.Date(2026, 8, dayOfMonth, 0, 0, 0, 0, time.UTC),
		State: state,
	}
}

// dates flattens a grid into its day numbers, for order assertions.
func dates(grid Grid) []int {
	var out []int
	for _, week := range grid.Weeks {
		for _, cell := range week {
			out = append(out, cell.Date.Day())
		}
	}
	return out
}

// ///////////////////////////////////////////////
// Shape
// ///////////////////////////////////////////////

func TestBuild_RowsAreWholeWeeks(t *testing.T) {
	// The grid is drawn as a table, so a short row would misalign every
	// column after it.
	tests := []struct {
		name      string
		year      int
		month     time.Month
		weekStart time.Weekday
	}{
		{name: "august 2026", year: 2026, month: time.August, weekStart: time.Sunday},
		{name: "february 2026", year: 2026, month: time.February, weekStart: time.Sunday},
		{name: "leap february 2028", year: 2028, month: time.February, weekStart: time.Sunday},
		{name: "monday start", year: 2026, month: time.August, weekStart: time.Monday},
		{name: "month starting on the week start", year: 2026, month: time.March, weekStart: time.Sunday},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grid := Build(tt.year, tt.month, tt.weekStart, nil, time.UTC)

			if len(grid.Weeks) == 0 {
				t.Fatal("Build() produced no weeks")
			}
			for i, week := range grid.Weeks {
				if len(week) != DaysPerWeek {
					t.Errorf("week %d has %d days, want %d", i, len(week), DaysPerWeek)
				}
			}
		})
	}
}

func TestBuild_StartsOnTheConfiguredWeekday(t *testing.T) {
	tests := []struct {
		name      string
		weekStart time.Weekday
	}{
		{name: "sunday", weekStart: time.Sunday},
		{name: "monday", weekStart: time.Monday},
		{name: "saturday", weekStart: time.Saturday},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grid := Build(2026, time.August, tt.weekStart, nil, time.UTC)

			for i, week := range grid.Weeks {
				if week[0].Date.Weekday() != tt.weekStart {
					t.Errorf("week %d starts on %s, want %s", i, week[0].Date.Weekday(), tt.weekStart)
				}
			}
		})
	}
}

func TestBuild_NeverStartsAfterTheFirst(t *testing.T) {
	// Aligning to the week start has to walk backwards. Subtracting
	// without wrapping walks forwards whenever the month opens on a
	// weekday earlier than the week start, which drops days off the front
	// of the grid. Sweeping every month and every week start catches it
	// without having to guess which combination triggers it.
	for offset := range 24 {
		first := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, offset, 0)

		for weekday := range DaysPerWeek {
			weekStart := time.Weekday(weekday)

			grid := Build(first.Year(), first.Month(), weekStart, nil, time.UTC)
			if len(grid.Weeks) == 0 {
				t.Fatalf("%s with week start %s produced no weeks", first.Format("2006-01"), weekStart)
			}

			opening := grid.Weeks[0][0].Date
			if opening.After(first) {
				t.Errorf("%s with week start %s opens on %s, want on or before the 1st",
					first.Format("2006-01"), weekStart, opening.Format("2006-01-02"))
			}
			if got := first.Sub(opening); got >= DaysPerWeek*24*time.Hour {
				t.Errorf("%s with week start %s opens %s before the 1st, want under a week",
					first.Format("2006-01"), weekStart, got)
			}

			last := grid.Weeks[len(grid.Weeks)-1][DaysPerWeek-1].Date
			endOfMonth := first.AddDate(0, 1, -1)
			if last.Before(endOfMonth) {
				t.Errorf("%s with week start %s ends on %s, want on or after %s",
					first.Format("2006-01"), weekStart,
					last.Format("2006-01-02"), endOfMonth.Format("2006-01-02"))
			}
		}
	}
}

func TestBuild_DaysAreConsecutive(t *testing.T) {
	grid := Build(2026, time.August, time.Sunday, nil, time.UTC)

	var previous time.Time
	for _, week := range grid.Weeks {
		for _, cell := range week {
			if !previous.IsZero() {
				if got := cell.Date.Sub(previous); got != 24*time.Hour {
					t.Fatalf("gap of %s between %s and %s, want one day",
						got, previous.Format("2006-01-02"), cell.Date.Format("2006-01-02"))
				}
			}
			previous = cell.Date
		}
	}
}

func TestBuild_CoversTheWholeMonth(t *testing.T) {
	grid := Build(2026, time.August, time.Sunday, nil, time.UTC)

	seen := make(map[int]bool)
	for _, week := range grid.Weeks {
		for _, cell := range week {
			if cell.InMonth {
				seen[cell.Date.Day()] = true
			}
		}
	}
	for d := 1; d <= 31; d++ {
		if !seen[d] {
			t.Errorf("August %d is missing from the grid", d)
		}
	}
	if len(seen) != 31 {
		t.Errorf("grid holds %d in-month days, want 31", len(seen))
	}
}

func TestBuild_MarksPaddingDays(t *testing.T) {
	// August 2026 starts on a Saturday, so a Sunday-start grid pads the
	// first row with six days of July.
	grid := Build(2026, time.August, time.Sunday, nil, time.UTC)

	first := grid.Weeks[0]
	for i := range 6 {
		if first[i].InMonth {
			t.Errorf("cell %d is marked in-month, want it padding from July", i)
		}
		if first[i].Date.Month() != time.July {
			t.Errorf("padding cell %d is in %s, want July", i, first[i].Date.Month())
		}
	}
	if !first[6].InMonth {
		t.Error("August 1 is marked as padding, want it in-month")
	}
}

func TestBuild_PaddingKeepsItsCoverage(t *testing.T) {
	// A missed broadcast on the last day of the preceding month must be
	// visible from this month's view, not hidden until the page turns back.
	july31 := store.Day{
		Date:  time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		State: store.CoverageMissed,
	}

	grid := Build(2026, time.August, time.Sunday, []store.Day{july31}, time.UTC)

	cell, ok := grid.Find(july31.Date)
	if !ok {
		t.Fatal("July 31 is not in the grid")
	}
	if cell.InMonth {
		t.Error("July 31 is marked in-month for an August grid")
	}
	if cell.Coverage != store.CoverageMissed {
		t.Errorf("July 31 coverage = %q, want it preserved as %q", cell.Coverage, store.CoverageMissed)
	}
}

// ///////////////////////////////////////////////
// Coverage
// ///////////////////////////////////////////////

func TestBuild_UnknownWhereThereIsNoData(t *testing.T) {
	// A day nothing looked at must not read as a day with no broadcast.
	grid := Build(2026, time.August, time.Sunday, nil, time.UTC)

	for _, week := range grid.Weeks {
		for _, cell := range week {
			if cell.Coverage != store.CoverageUnknown {
				t.Fatalf("%s coverage = %q, want %q with no data",
					cell.Date.Format("2006-01-02"), cell.Coverage, store.CoverageUnknown)
			}
		}
	}
}

func TestBuild_CarriesCoverageDetail(t *testing.T) {
	days := []store.Day{{
		Date:       time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		State:      store.CoveragePartial,
		Broadcasts: 2,
		Captured:   1,
		Bytes:      13_450_000_000,
	}}

	grid := Build(2026, time.August, time.Sunday, days, time.UTC)

	cell, ok := grid.Find(days[0].Date)
	if !ok {
		t.Fatal("August 12 is not in the grid")
	}
	if cell.Coverage != store.CoveragePartial {
		t.Errorf("Coverage = %q, want %q", cell.Coverage, store.CoveragePartial)
	}
	if cell.Broadcasts != 2 || cell.Captured != 1 {
		t.Errorf("Broadcasts/Captured = %d/%d, want 2/1", cell.Broadcasts, cell.Captured)
	}
	if cell.Bytes != 13_450_000_000 {
		t.Errorf("Bytes = %d, want the day's total", cell.Bytes)
	}
}

// ///////////////////////////////////////////////
// Summarize
// ///////////////////////////////////////////////

func TestGrid_Summarize(t *testing.T) {
	days := []store.Day{
		day(3, store.CoverageLive),
		day(4, store.CoverageLive),
		day(5, store.CoverageRecovered),
		day(6, store.CoveragePartial),
		day(7, store.CoverageMissed),
		day(8, store.CoverageMissed),
		day(9, store.CoverageNoStream),
	}
	days[0].Bytes = 1000
	days[1].Bytes = 2000

	summary := Build(2026, time.August, time.Sunday, days, time.UTC).Summarize()

	tests := []struct {
		name string
		got  int
		want int
	}{
		{name: "live", got: summary.Live, want: 2},
		{name: "recovered", got: summary.Recovered, want: 1},
		{name: "partial", got: summary.Partial, want: 1},
		{name: "missed", got: summary.Missed, want: 2},
		{name: "no stream", got: summary.NoStream, want: 1},
		{name: "unknown", got: summary.Unknown, want: 31 - 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}

	if summary.Bytes != 3000 {
		t.Errorf("Bytes = %d, want 3000", summary.Bytes)
	}
}

func TestGrid_SummarizeExcludesPadding(t *testing.T) {
	// The figures describe the month on display, not the rectangle it is
	// drawn in, so a missed day in July must not inflate August's count.
	july31 := store.Day{
		Date:  time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		State: store.CoverageMissed,
	}

	summary := Build(2026, time.August, time.Sunday, []store.Day{july31}, time.UTC).Summarize()

	if summary.Missed != 0 {
		t.Errorf("Missed = %d, want 0 for a July day in an August summary", summary.Missed)
	}
}

// ///////////////////////////////////////////////
// Gaps
// ///////////////////////////////////////////////

func TestGrid_Gaps(t *testing.T) {
	days := []store.Day{
		day(3, store.CoverageLive),
		day(5, store.CoverageMissed),
		day(7, store.CoveragePartial),
		day(9, store.CoverageNoStream),
		day(11, store.CoverageUnknown),
	}

	gaps := Build(2026, time.August, time.Sunday, days, time.UTC).Gaps()

	if len(gaps) != 2 {
		t.Fatalf("Gaps() returned %d, want the missed and partial days only: %v", len(gaps), gaps)
	}
	if gaps[0].Date.Day() != 5 || gaps[1].Date.Day() != 7 {
		t.Errorf("Gaps() = days %d and %d, want 5 and 7 in order",
			gaps[0].Date.Day(), gaps[1].Date.Day())
	}
}

func TestGrid_GapsExcludesPadding(t *testing.T) {
	july31 := store.Day{
		Date:  time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		State: store.CoverageMissed,
	}

	if gaps := Build(2026, time.August, time.Sunday, []store.Day{july31}, time.UTC).Gaps(); len(gaps) != 0 {
		t.Errorf("Gaps() = %v, want none from padding", gaps)
	}
}

// ///////////////////////////////////////////////
// Labels
// ///////////////////////////////////////////////

func TestGrid_Title(t *testing.T) {
	if got := Build(2026, time.August, time.Sunday, nil, time.UTC).Title(); got != "August 2026" {
		t.Errorf("Title() = %q, want %q", got, "August 2026")
	}
}

func TestGrid_WeekdayHeadings(t *testing.T) {
	tests := []struct {
		name      string
		weekStart time.Weekday
		want      []string
	}{
		{
			name:      "sunday start",
			weekStart: time.Sunday,
			want:      []string{"Su", "Mo", "Tu", "We", "Th", "Fr", "Sa"},
		},
		{
			name:      "monday start",
			weekStart: time.Monday,
			want:      []string{"Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(2026, time.August, tt.weekStart, nil, time.UTC).WeekdayHeadings()

			if len(got) != len(tt.want) {
				t.Fatalf("WeekdayHeadings() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("WeekdayHeadings()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ///////////////////////////////////////////////
// Find
// ///////////////////////////////////////////////

func TestGrid_Find(t *testing.T) {
	grid := Build(2026, time.August, time.Sunday, nil, time.UTC)

	if _, ok := grid.Find(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)); !ok {
		t.Error("Find() did not locate August 12")
	}
	if _, ok := grid.Find(time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)); ok {
		t.Error("Find() located a date outside the grid")
	}
}

func TestBuild_NilLocationFallsBackToUTC(t *testing.T) {
	grid := Build(2026, time.August, time.Sunday, nil, nil)

	if len(grid.Weeks) == 0 {
		t.Fatal("Build() produced no weeks")
	}
	if got := grid.Weeks[0][0].Date.Location(); got != time.UTC {
		t.Errorf("cell location = %s, want UTC", got)
	}
}

func TestBuild_OrdersDaysAscending(t *testing.T) {
	got := dates(Build(2026, time.August, time.Sunday, nil, time.UTC))

	if len(got) == 0 {
		t.Fatal("grid is empty")
	}
	if got[6] != 1 {
		t.Errorf("seventh cell is day %d, want August 1", got[6])
	}
}

// TestBuild_AZoneThatMovesItsClocksAtMidnightGetsEachDayOnce covers the
// grid walk in zones whose day does not begin at 00:00.
//
// Santiago moves its clocks forward at local midnight, so the instant
// 2022-09-11 00:00 does not exist there. A walk that adds a day to a local
// time lands inside the day it left. That repeats a date and carries the
// previous evening's hour through every later cell. The grid then paints a
// day as missed while the fetch behind it queries a different 24 hours,
// finds nothing, and reports no error.
func TestBuild_AZoneThatMovesItsClocksAtMidnightGetsEachDayOnce(t *testing.T) {
	zones := []struct {
		name  string
		zone  string
		year  int
		month time.Month
	}{
		{"Santiago moves forward at midnight", "America/Santiago", 2022, time.September},
		{"Havana moves forward at midnight", "America/Havana", 2023, time.March},
		{"Beirut moves forward at midnight", "Asia/Beirut", 2023, time.March},
	}

	for _, tt := range zones {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Skipf("this system has no tzdata for %s: %v", tt.zone, err)
			}

			grid := Build(tt.year, tt.month, time.Sunday, nil, loc)

			seen := map[string]int{}
			for _, week := range grid.Weeks {
				if len(week) != DaysPerWeek {
					t.Fatalf("week has %d cells, want %d", len(week), DaysPerWeek)
				}
				for _, c := range week {
					seen[c.Date.Format("2006-01-02")]++
				}
			}

			for date, count := range seen {
				if count != 1 {
					t.Errorf("%s appears %d times in the grid, want once", date, count)
				}
			}
			// Every cell must be the first instant of its own day, or the
			// range a fetch derives from it starts in the wrong place.
			for _, week := range grid.Weeks {
				for _, c := range week {
					y, m, d := c.Date.Date()
					if want := store.StartOfDayOn(y, m, d, loc); !c.Date.Equal(want) {
						t.Errorf("cell %s is %s, want the day to start at %s",
							c.Date.Format("2006-01-02"), c.Date, want)
					}
				}
			}
		})
	}
}
