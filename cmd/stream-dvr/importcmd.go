package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/importer"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// library import
// ///////////////////////////////////////////////

// importDetail is how many refusals are spelled out before the rest are
// summarized.
//
// A library adopted from somewhere else can hold thousands of files the
// template cannot read, and printing a line for each buries the ones that
// were imported. The count that follows says how many were not shown, so a
// truncated list never reads as the whole answer.
const importDetail = 10

// dispositionOutcome maps what happened to a file onto how it is drawn.
var dispositionOutcome = map[importer.Disposition]outcome{
	importer.Restored: outcomePass,
	importer.Inferred: outcomeWarn,
	importer.Skipped:  outcomeNote,
	importer.Refused:  outcomeFail,
}

// dispositionLabel names each outcome in the operator's terms.
var dispositionLabel = map[importer.Disposition]string{
	importer.Restored: "restored",
	importer.Inferred: "read from its name",
	importer.Skipped:  "skipped",
	importer.Refused:  "not imported",
}

// runImport adopts library files no recording row names.
func runImport(ctx context.Context, out io.Writer, configPath, channel string, dryRun bool) error {
	out = styled(out)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		return err
	}
	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		return err
	}
	defer db.Close()

	// The template that reads a name back is the one that wrote it. Loading
	// the configured template rather than the default is what lets an
	// operator who changed theirs import the library they actually have.
	template, err := naming.Parse(cfg.Naming.Template)
	if err != nil {
		return err
	}
	location, err := cfg.Location()
	if err != nil {
		return err
	}

	report, err := importer.New(lib, db, post.New(), template, location, importer.Options{
		DryRun:     dryRun,
		Channel:    channel,
		Configured: cfg.Channels,
	}).Run(ctx)
	if err != nil {
		return err
	}

	reportImport(out, report)
	return nil
}

// reportImport draws what an import amounted to.
func reportImport(out io.Writer, report importer.Report) {
	if len(report.Files) == 0 {
		section(out, "import", []row{{
			State:   outcomeNote,
			Label:   "library",
			Trailer: "holds no media the recordings do not already name",
		}})
		return
	}

	section(out, "import", importCounts(report))

	if detail := importDetails(report); len(detail) > 0 {
		section(out, "not imported", detail)
	}
	summary(out, importSummary(report), importNext(report))
}

// importCounts is one row per disposition, naming only what happened.
//
// A disposition nothing landed in is left out rather than drawn as a zero. An
// import into a library with no sidecars would otherwise lead with a row
// saying nothing was restored.
func importCounts(report importer.Report) []row {
	counts := make([]row, 0, len(dispositionLabel))
	for _, disposition := range []importer.Disposition{
		importer.Restored, importer.Inferred, importer.Skipped, importer.Refused,
	} {
		count := report.Count(disposition)
		if count == 0 {
			continue
		}
		counts = append(counts, row{
			State:   dispositionOutcome[disposition],
			Label:   dispositionLabel[disposition],
			Trailer: fmt.Sprintf("%d %s", count, plural(count, "file", "files")),
		})
	}
	return counts
}

// importDetails spells out the files that were refused, and the readings that
// carry a caveat.
//
// Both are things the operator may want to act on: a refusal is a file still
// invisible to the library, and a caveat is a row whose metadata is a guess.
// A clean restore says nothing, because there is nothing to decide about it.
func importDetails(report importer.Report) []row {
	detail := make([]row, 0, importDetail)
	var hidden int

	for _, file := range report.Files {
		note := noteFor(file)
		if note == "" {
			continue
		}
		if len(detail) == importDetail {
			hidden++
			continue
		}
		detail = append(detail, row{
			State:   dispositionOutcome[file.Disposition],
			Trailer: note,
			Path:    escape.Text(file.Path),
		})
	}

	if hidden > 0 {
		detail = append(detail, row{
			State:   outcomeNote,
			Label:   "and",
			Trailer: fmt.Sprintf("%d more not shown", hidden),
		})
	}
	return detail
}

// noteFor is what one file has to say for itself, or "" for a file that needs
// no explaining.
func noteFor(file importer.File) string {
	if file.Disposition == importer.Skipped {
		return ""
	}
	parts := make([]string, 0, 2)
	if file.Reason != "" {
		parts = append(parts, file.Reason)
	}
	parts = append(parts, file.Disagreements...)
	return escape.Text(strings.Join(parts, "; "))
}

// importSummary counts what the run did.
func importSummary(report importer.Report) string {
	imported := report.Imported()
	counted := fmt.Sprintf("%d of %d %s", imported, len(report.Files),
		plural(len(report.Files), "file", "files"))
	if report.DryRun {
		// Stated in the count rather than only in a hint, because the count
		// is the line an operator reads first and "imported" would be false.
		return counted + " would be imported"
	}
	return counted + " imported"
}

// importNext is the one thing worth doing after an import, or "".
func importNext(report importer.Report) string {
	switch {
	case report.DryRun:
		return "run this without --dry-run to import them"
	case report.Count(importer.Inferred) > 0:
		return "a recording read from its name carries a title and a date nobody verified"
	case report.Count(importer.Refused) > 0:
		return "a file that was not imported is still exactly where it was"
	}
	return ""
}
