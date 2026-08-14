package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/importer"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// drawn renders an import report the way the command would.
func drawn(t *testing.T, report importer.Report) string {
	t.Helper()

	var out bytes.Buffer
	reportImport(&out, report)
	return ansi.Strip(out.String())
}

// ///////////////////////////////////////////////
// Counts
// ///////////////////////////////////////////////

func TestReportImport_NamesOnlyWhatHappened(t *testing.T) {
	// A disposition nothing landed in is left out. An import into a library
	// with no sidecars would otherwise lead with a row announcing that
	// nothing was restored, which is not a result.
	view := drawn(t, importer.Report{Files: []importer.File{
		{Path: "a/one.mkv", Disposition: importer.Inferred, RecordingID: 1},
		{Path: "a/two.mkv", Disposition: importer.Inferred, RecordingID: 2},
	}})

	if !strings.Contains(view, "read from its name") {
		t.Errorf("output does not name what happened:\n%s", view)
	}
	if strings.Contains(view, "restored") {
		t.Errorf("output reports a disposition nothing landed in:\n%s", view)
	}
	if !strings.Contains(view, "2 files") {
		t.Errorf("output does not count them:\n%s", view)
	}
}

func TestReportImport_SaysWhenThereWasNothingToDo(t *testing.T) {
	// An empty section reads as a failure to look. The operator ran this to
	// find out whether anything was unrecorded, and "nothing was" is the
	// answer.
	view := drawn(t, importer.Report{})

	if !strings.Contains(view, "already name") {
		t.Errorf("output does not say the library was already complete:\n%s", view)
	}
}

// ///////////////////////////////////////////////
// Detail
// ///////////////////////////////////////////////

func TestReportImport_SpellsOutWhatWasNotImported(t *testing.T) {
	// A refused file is still invisible to the library, so the operator has
	// to be able to see which ones and why.
	view := drawn(t, importer.Report{Files: []importer.File{
		{Path: "a/mystery.mkv", Disposition: importer.Refused, Reason: "does not fit the naming template"},
	}})

	for _, want := range []string{"mystery.mkv", "naming template", "not imported"} {
		if !strings.Contains(view, want) {
			t.Errorf("output does not mention %q:\n%s", want, view)
		}
	}
}

func TestReportImport_CarriesADisagreementIntoTheOutput(t *testing.T) {
	// A sidecar contradicted by its file is the case that made measurement
	// mandatory. Storing the measured value and never saying so would leave
	// the operator with a corrected row and no idea it was corrected.
	view := drawn(t, importer.Report{Files: []importer.File{
		{
			Path:          "a/one.mkv",
			Disposition:   importer.Restored,
			RecordingID:   1,
			Disagreements: []string{"its sidecar claims 0B and the file holds 9.9GB"},
		},
	}})

	if !strings.Contains(view, "9.9GB") {
		t.Errorf("output drops the disagreement:\n%s", view)
	}
}

func TestReportImport_SaysNothingAboutACleanSkip(t *testing.T) {
	// A file the library already names needs no explaining, and a line per
	// skip buries the files that do.
	view := drawn(t, importer.Report{Files: []importer.File{
		{Path: "a/one.mkv", Disposition: importer.Skipped, Reason: "a recording already names this file"},
	}})

	if strings.Contains(view, "already names this file") {
		t.Errorf("output explains a skip that needs no explaining:\n%s", view)
	}
	if !strings.Contains(view, "skipped") {
		t.Errorf("output does not count the skip:\n%s", view)
	}
}

func TestReportImport_BoundsTheDetailAndSaysItDid(t *testing.T) {
	// A library adopted from elsewhere can hold thousands the template
	// cannot read. Printing them all buries the result, and truncating
	// silently reads as though that was all of them.
	var files []importer.File
	for range importDetail + 5 {
		files = append(files, importer.File{
			Path: "a/one.mkv", Disposition: importer.Refused, Reason: "does not fit",
		})
	}

	view := drawn(t, importer.Report{Files: files})

	if !strings.Contains(view, "5 more not shown") {
		t.Errorf("output does not say what it left out:\n%s", view)
	}
	if got := strings.Count(view, "does not fit"); got != importDetail {
		t.Errorf("output spelled out %d files, want %d", got, importDetail)
	}
}

// ///////////////////////////////////////////////
// Summary
// ///////////////////////////////////////////////

func TestReportImport_ADryRunNeverClaimsItImportedAnything(t *testing.T) {
	// The count is the line an operator reads first, so the tense has to be
	// right there rather than only in the hint below it.
	view := drawn(t, importer.Report{
		DryRun: true,
		Files: []importer.File{
			{Path: "a/one.mkv", Disposition: importer.Restored},
		},
	})

	if !strings.Contains(view, "would be imported") {
		t.Errorf("a dry run reports work it did not do:\n%s", view)
	}
	if !strings.Contains(view, "--dry-run") {
		t.Errorf("a dry run does not say how to commit it:\n%s", view)
	}
}

func TestReportImport_WarnsThatAReadingIsNotARecord(t *testing.T) {
	// A row read from a filename carries a title and a date nobody checked.
	// The operator has to be told once, where they will see it.
	view := drawn(t, importer.Report{Files: []importer.File{
		{Path: "a/one.mkv", Disposition: importer.Inferred, RecordingID: 1},
	}})

	if !strings.Contains(view, "nobody verified") {
		t.Errorf("output does not caveat an inferred row:\n%s", view)
	}
}

func TestReportImport_SaysNothingExtraWhenEverythingRestoredCleanly(t *testing.T) {
	// Every restore came from a record. There is nothing to act on, and a
	// caveat printed anyway is one the operator learns to skip.
	view := drawn(t, importer.Report{Files: []importer.File{
		{Path: "a/one.mkv", Disposition: importer.Restored, RecordingID: 1},
	}})

	for _, unwanted := range []string{"nobody verified", "still exactly where it was", "--dry-run"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("output carries %q with nothing to act on:\n%s", unwanted, view)
		}
	}
}

func TestNoteFor_JoinsTheReasonAndTheDisagreements(t *testing.T) {
	got := noteFor(importer.File{
		Disposition:   importer.Restored,
		Reason:        "title history was not restored",
		Disagreements: []string{"size disagrees", "length disagrees"},
	})

	for _, want := range []string{"title history", "size disagrees", "length disagrees"} {
		if !strings.Contains(got, want) {
			t.Errorf("noteFor() = %q, want it to carry %q", got, want)
		}
	}
}

func TestNoteFor_EscapesAPathsWorthOfControlCharacters(t *testing.T) {
	// A refusal carries the reason, and a reason can carry a filename
	// somebody else chose. This reaches a terminal.
	got := noteFor(importer.File{
		Disposition: importer.Refused,
		Reason:      "channel \x1b[2Jwiped\r\n is not known",
	})

	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("noteFor() = %q, want the control characters escaped", got)
	}
}
