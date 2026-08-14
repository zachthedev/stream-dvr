package tui

import (
	"testing"

	"charm.land/lipgloss/v2"

	"zach.tools/go/stream-dvr/internal/store"
)

// coverageStates lists every state the calendar can paint, from the store
// that defines them. Enumerating them here instead would agree until the day
// somebody adds a state, which is the only day this check matters.
var coverageStates = store.Coverages()

// ///////////////////////////////////////////////
// Completeness
// ///////////////////////////////////////////////

func TestForCoverage_CoversEveryState(t *testing.T) {
	// A state with no style falls back to dim, which would silently hide a
	// missed day among the settled ones.
	for _, coverage := range coverageStates {
		t.Run(string(coverage), func(t *testing.T) {
			if _, ok := newStyles(true).coverage[coverage]; !ok {
				t.Errorf("no style declared for %q", coverage)
			}
			if _, unset := newStyles(true).forCoverage(coverage).GetForeground().(lipgloss.NoColor); unset {
				t.Errorf("style for %q has no foreground", coverage)
			}
		})
	}
}

func TestLabelFor_CoversEveryState(t *testing.T) {
	for _, coverage := range coverageStates {
		t.Run(string(coverage), func(t *testing.T) {
			if labelFor(coverage) == "" {
				t.Errorf("no label for %q", coverage)
			}
		})
	}
}

func TestLabelFor_UnknownSaysWhatItMeans(t *testing.T) {
	// "unknown" is the one state whose stored name misleads: it sounds
	// like a data problem when it means nothing was watching. The label
	// has to state the fact instead.
	got := labelFor(store.CoverageUnknown)

	if got == string(store.CoverageUnknown) {
		t.Errorf("labelFor(unknown) = %q, want it to say nothing was watching", got)
	}
	if got != "not watched" {
		t.Errorf("labelFor(unknown) = %q, want %q", got, "not watched")
	}
}

func TestLegendOrder_ListsEveryStateWorstFirst(t *testing.T) {
	// Missed leads so the eye lands on what needs attention.
	if legendOrder[0] != store.CoverageMissed {
		t.Errorf("legendOrder starts with %q, want %q", legendOrder[0], store.CoverageMissed)
	}
	if legendOrder[1] != store.CoveragePartial {
		t.Errorf("legendOrder[1] = %q, want %q", legendOrder[1], store.CoveragePartial)
	}
	if legendOrder[2] != store.CoverageAtRisk {
		t.Errorf("legendOrder[2] = %q, want %q", legendOrder[2], store.CoverageAtRisk)
	}

	seen := make(map[store.Coverage]bool, len(legendOrder))
	for _, coverage := range legendOrder {
		if seen[coverage] {
			t.Errorf("legendOrder repeats %q", coverage)
		}
		seen[coverage] = true
	}
	for _, coverage := range coverageStates {
		if !seen[coverage] {
			t.Errorf("legendOrder omits %q", coverage)
		}
	}
}

// ///////////////////////////////////////////////
// Contrast
// ///////////////////////////////////////////////

func TestNewStyles_GreysFollowTheTerminalScheme(t *testing.T) {
	light, dark := newStyles(false), newStyles(true)

	assertScheme(t, "dim", light.dim, dark.dim)
	assertScheme(t, "padding", light.padding, dark.padding)
	assertScheme(t, "no broadcast",
		light.forCoverage(store.CoverageNoStream), dark.forCoverage(store.CoverageNoStream))
	assertScheme(t, "not watched",
		light.forCoverage(store.CoverageUnknown), dark.forCoverage(store.CoverageUnknown))
}

func TestNewStyles_RecordsTheSchemeItResolvedAgainst(t *testing.T) {
	// The line editor takes its own set for the same scheme, so the flag
	// has to survive on the struct rather than only in the colours.
	if newStyles(true).dark != true {
		t.Error("newStyles(true).dark = false, want the scheme it was built for")
	}
	if newStyles(false).dark != false {
		t.Error("newStyles(false).dark = true, want the scheme it was built for")
	}
}

func TestNewStyles_DimRecedesByColourRatherThanFaint(t *testing.T) {
	assertRecedesByColour(t, newStyles(true).dim)
}

func TestNewStyles_PaddingRecedesByColourRatherThanFaint(t *testing.T) {
	assertRecedesByColour(t, newStyles(true).padding)
}

// assertScheme checks that a grey differs between the two terminal schemes.
//
// One fixed grey has to be wrong on either a light or a dark background.
// Bright black renders near #93a1a1 on Solarized Light's #fdf6e3, well under
// 3:1, and a fixed 256-colour grey ignores the scheme altogether.
func assertScheme(t *testing.T, name string, light, dark lipgloss.Style) {
	t.Helper()

	lightColor, darkColor := light.GetForeground(), dark.GetForeground()
	if _, unset := lightColor.(lipgloss.NoColor); unset {
		t.Errorf("%s has no foreground on a light background", name)
	}
	if _, unset := darkColor.(lipgloss.NoColor); unset {
		t.Errorf("%s has no foreground on a dark background", name)
	}
	if lightColor == darkColor {
		t.Errorf("%s is %v on both schemes, which is a fixed colour wearing an adaptive one",
			name, lightColor)
	}
}

// assertRecedesByColour checks that a style holding a fact is not Faint.
//
// Many terminals implement Faint as a further opacity cut on top of an
// already grey foreground, and every style checked here carries a fact
// rather than decoration: a recorder condition, a capture count, a date
// belonging to a neighbouring month.
func assertRecedesByColour(t *testing.T, style lipgloss.Style) {
	t.Helper()

	if style.GetFaint() {
		t.Error("style is Faint, want it to recede by colour alone")
	}
	if _, unset := style.GetForeground().(lipgloss.NoColor); unset {
		t.Error("style has no foreground, so Faint was the only thing making it recede")
	}
}

// ///////////////////////////////////////////////
// Fallbacks
// ///////////////////////////////////////////////

func TestForCoverage_UnknownStateFallsBack(t *testing.T) {
	// A state added to the store without a style here must still render.
	if got := newStyles(true).forCoverage(store.Coverage("invented")); got.GetBold() {
		t.Error("forCoverage() gave an unknown state an emphatic style, want it to recede")
	}
}

func TestLabelFor_UnknownStateFallsBackToItsName(t *testing.T) {
	if got := labelFor(store.Coverage("invented")); got != "invented" {
		t.Errorf("labelFor() = %q, want the raw state name as a fallback", got)
	}
}

// ///////////////////////////////////////////////
// Emphasis
// ///////////////////////////////////////////////

func TestNewStyles_OnlyActionableStatesAreEmphasized(t *testing.T) {
	// A missed broadcast, a partly captured day, and a capture that has not
	// reached the library are the states worth acting on. Emphasizing
	// anything else would flatten the grid into noise.
	emphatic := map[store.Coverage]bool{
		store.CoverageMissed:  true,
		store.CoveragePartial: true,
		store.CoverageAtRisk:  true,
	}

	for coverage, style := range newStyles(true).coverage {
		if style.GetBold() != emphatic[coverage] {
			t.Errorf("%q bold = %t, want %t", coverage, style.GetBold(), emphatic[coverage])
		}
	}
}
