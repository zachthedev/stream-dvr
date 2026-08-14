package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/calendar"
)

// headings are the weekday labels a Sunday-start grid inlays into its top
// rule.
var headings = []string{"Su", "Mo", "Tu", "We", "Th", "Fr", "Sa"}

// ///////////////////////////////////////////////
// Junctions
// ///////////////////////////////////////////////

func TestJunctions_CoverEveryCombinationOfTwoOrMoreArms(t *testing.T) {
	// A heavy cursor inside a light lattice needs mixed junctions, and every
	// one of them has to be a real glyph rather than an approximation. There
	// are seventy-two: six arm pairs times four weightings, four triples
	// times eight, and one quadruple times sixteen.
	weights := []weight{none, light, heavy}

	want := 0
	for _, up := range weights {
		for _, right := range weights {
			for _, down := range weights {
				for _, left := range weights {
					set := arms{up, right, down, left}
					if drawn(set) < 2 {
						continue
					}
					want++
					if _, ok := junctions[set]; !ok {
						t.Errorf("no glyph for %v", set)
					}
				}
			}
		}
	}

	if want != 72 {
		t.Fatalf("counted %d combinations, want 72", want)
	}
	if len(junctions) != want {
		t.Errorf("the table holds %d glyphs, want %d", len(junctions), want)
	}
}

func TestJunctions_NoGlyphIsClaimedTwice(t *testing.T) {
	// Two arm sets mapping to one glyph means one of them is wrong, and the
	// wrong one draws a border the eye reads as a different weight.
	seen := make(map[rune]arms, len(junctions))
	for set, glyph := range junctions {
		if first, ok := seen[glyph]; ok {
			t.Errorf("%q is claimed by both %v and %v", glyph, first, set)
		}
		seen[glyph] = set
	}
}

func TestJunctions_EveryGlyphIsOneCellWide(t *testing.T) {
	// A junction drawn at two cells would step the whole lattice out of true
	// from that column on.
	for set, glyph := range junctions {
		if got := ansi.StringWidth(string(glyph)); got != 1 {
			t.Errorf("%v draws %q at %d cells, want 1", set, glyph, got)
		}
	}
}

func TestJunction_TheCursorsFourCornersAreReal(t *testing.T) {
	// The four mixed corners a heavy cell needs where it meets the light
	// lattice. Named rather than derived, because these are the ones the
	// design turns on.
	tests := []struct {
		name                  string
		up, right, down, left weight
		want                  rune
	}{
		{"upper left of the cursor", light, heavy, heavy, light, '╆'},
		{"upper right of the cursor", light, light, heavy, heavy, '╅'},
		{"lower left of the cursor", heavy, heavy, light, light, '╄'},
		{"lower right of the cursor", heavy, light, light, heavy, '╃'},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := junction(tt.up, tt.right, tt.down, tt.left); got != tt.want {
				t.Errorf("junction() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJunction_FewerThanTwoArmsDrawsNothing(t *testing.T) {
	// A stub is not a junction. Answering with a space keeps the lattice
	// rectangular rather than one column short on that row.
	if got := junction(none, none, none, none); got != ' ' {
		t.Errorf("junction() with no arms = %q, want a space", got)
	}
	if got := junction(none, light, none, none); got != ' ' {
		t.Errorf("junction() with one arm = %q, want a space", got)
	}
}

// ///////////////////////////////////////////////
// The grid
// ///////////////////////////////////////////////

func TestDrawGrid_MatchesTheDesign(t *testing.T) {
	// The drawn form of the design, cell for cell. The cursor sits on Friday
	// 14 August, so the two rules around it and the two verticals beside it
	// carry the four mixed corners.
	want := []string{
		"┌─Su──┬─Mo──┬─Tu──┬─We──┬─Th──┬─Fr──┬─Sa──┐",
		"│(26).│(27)#│(28).│(29)-│(30)#│(31)!│  1 #│",
		"├─────┼─────┼─────┼─────┼─────┼─────┼─────┤",
		"│  2 #│  3 -│  4 #│  5 !│  6 #│  7 ~│  8 #│",
		"├─────┼─────┼─────┼─────┼─────╆━━━━━╅─────┤",
		"│  9 #│ 10 #│ 11 -│ 12 #│ 13 !┃ 14 ~┃ 15 #│",
		"├─────┼─────┼─────┼─────┼─────╄━━━━━╃─────┤",
		"│ 16 +│ 17 >│ 18*#│ 19 .│ 20 .│ 21 .│ 22 .│",
		"├─────┼─────┼─────┼─────┼─────┼─────┼─────┤",
		"│ 23 .│ 24 .│ 25 .│ 26 .│ 27 .│ 28 .│ 29 .│",
		"├─────┼─────┼─────┼─────┼─────┼─────┼─────┤",
		"│ 30 .│ 31 .│( 1).│( 2).│( 3).│( 4).│( 5).│",
		"└─────┴─────┴─────┴─────┴─────┴─────┴─────┘",
	}

	got := drawGrid(headings, augustFaces(), cursor{Week: 2, Day: 5}, false)

	assertLines(t, got, want)
}

func TestDrawGrid_ADegradedRangeDashesTheOuterFrame(t *testing.T) {
	// Degraded is a property of the whole range, so it marks the frame and
	// not a cell. Corners and junctions stay solid: Unicode has no dashed
	// junctions, and an approximated one reads as a different weight.
	want := []string{
		"┌┄Su┄┄┬┄Mo┄┄┬┄Tu┄┄┬┄We┄┄┬┄Th┄┄┬┄Fr┄┄┬┄Sa┄┄┐",
		"┆(26).│(27)#│(28).│(29)-│(30)#│(31)!│  1 #┆",
		"├─────┼─────┼─────┼─────┼─────┼─────┼─────┤",
		"┆  2 #│  3 -│  4 #│  5 !│  6 #│  7 ~│  8 #┆",
		"├─────┼─────┼─────┼─────┼─────╆━━━━━╅─────┤",
		"┆  9 #│ 10 #│ 11 -│ 12 #│ 13 !┃ 14 ~┃ 15 #┆",
		"├─────┼─────┼─────┼─────┼─────╄━━━━━╃─────┤",
		"┆ 16 +│ 17 >│ 18*#│ 19 .│ 20 .│ 21 .│ 22 .┆",
		"├─────┼─────┼─────┼─────┼─────┼─────┼─────┤",
		"┆ 23 .│ 24 .│ 25 .│ 26 .│ 27 .│ 28 .│ 29 .┆",
		"├─────┼─────┼─────┼─────┼─────┼─────┼─────┤",
		"┆ 30 .│ 31 .│( 1).│( 2).│( 3).│( 4).│( 5).┆",
		"└┄┄┄┄┄┴┄┄┄┄┄┴┄┄┄┄┄┴┄┄┄┄┄┴┄┄┄┄┄┴┄┄┄┄┄┴┄┄┄┄┄┘",
	}

	got := drawGrid(headings, augustFaces(), cursor{Week: 2, Day: 5}, true)

	assertLines(t, got, want)
}

func TestDrawGrid_EveryLineIsTheGridsWidth(t *testing.T) {
	// The panel beside it is sized from this constant. A row one cell out
	// splits the frame from that row down.
	for _, degraded := range []bool{false, true} {
		for week := range 6 {
			for day := range calendar.DaysPerWeek {
				lines := drawGrid(headings, augustFaces(), cursor{Week: week, Day: day}, degraded)
				for i, line := range lines {
					if got := ansi.StringWidth(line); got != gridColumns {
						t.Fatalf("cursor at %d,%d degraded=%t: line %d is %d cells, want %d\n%s",
							week, day, degraded, i, got, gridColumns, line)
					}
				}
			}
		}
	}
}

func TestDrawGrid_WithNoCursorTheLatticeIsAllLight(t *testing.T) {
	// A grid drawn before the cursor lands anywhere must not show a heavy
	// border nobody put there.
	lines := drawGrid(headings, augustFaces(), cursor{Week: -1, Day: -1}, false)

	for i, line := range lines {
		if strings.ContainsAny(line, string(heavyHorizontal)+string(heavyVertical)) {
			t.Errorf("line %d carries a heavy run with no cursor set: %q", i, line)
		}
	}
}

func TestDrawGrid_TheCursorIsDrawnWithoutColour(t *testing.T) {
	// Border weight is the cursor, so an operator on NO_COLOR or a
	// monochrome terminal still knows which day they are about to act on.
	lines := drawGrid(headings, augustFaces(), cursor{Week: 2, Day: 5}, false)

	joined := strings.Join(lines, "\n")
	if !strings.ContainsRune(joined, heavyVertical) {
		t.Fatal("no heavy vertical was drawn, so the cursor is invisible without colour")
	}
	if !strings.ContainsRune(joined, heavyHorizontal) {
		t.Fatal("no heavy horizontal was drawn, so the cursor is invisible without colour")
	}
}

func TestDrawGrid_NoWeeksDrawsNothing(t *testing.T) {
	if got := drawGrid(headings, nil, cursor{Week: -1, Day: -1}, false); got != nil {
		t.Errorf("drawGrid() with no weeks = %v, want nil", got)
	}
}

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// drawn counts how many of a set's arms are there.
func drawn(set arms) int {
	count := 0
	for _, w := range []weight{set.Up, set.Right, set.Down, set.Left} {
		if w != none {
			count++
		}
	}
	return count
}

// augustFaces is the design's fixture month: August 2026 on a Sunday start,
// with today on the 18th.
func augustFaces() [][]face {
	rows := []string{
		"(26).(27)#(28).(29)-(30)#(31)!  1 #",
		"  2 #  3 -  4 #  5 !  6 #  7 ~  8 #",
		"  9 # 10 # 11 - 12 # 13 ! 14 ~ 15 #",
		" 16 + 17 > 18*# 19 . 20 . 21 . 22 .",
		" 23 . 24 . 25 . 26 . 27 . 28 . 29 .",
		" 30 . 31 .( 1).( 2).( 3).( 4).( 5).",
	}

	faces := make([][]face, 0, len(rows))
	for _, row := range rows {
		week := make([]face, 0, calendar.DaysPerWeek)
		for day := range calendar.DaysPerWeek {
			week = append(week, face{
				Text:  row[day*gridInterior : (day+1)*gridInterior],
				Style: lipgloss.NewStyle(),
			})
		}
		faces = append(faces, week)
	}
	return faces
}

// assertLines compares a rendered block line by line.
func assertLines(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("drew %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}
