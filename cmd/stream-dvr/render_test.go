package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/post"
)

// pinned is an output at a width a test chose.
//
// The renderer measures a terminal it can reach and assumes 80 columns
// otherwise, so a test that wants to see what a 40-column terminal gets has
// to say so.
type pinned struct {
	bytes.Buffer
	columns int
}

// joined renders a result row the way a section would, for a test that
// asserts on the whole line rather than on one field of it.
func joined(r row) string {
	return strings.Join([]string{mark(r.State), r.Label, r.Trailer, r.Path}, " ")
}

// ///////////////////////////////////////////////
// The result row
// ///////////////////////////////////////////////

func TestRunDoctor_EveryRowFitsTheTerminal(t *testing.T) {
	// A row that overruns wraps into a ragged second line, which is what a
	// 175-column dependency row did at every terminal narrower than itself.
	//
	// Rows only. A path and a command are lines an operator copies or types,
	// and cutting either produces something that does not work, so those
	// wrap instead. TestRunDoctor_APathOnItsOwnLineIsNeverCut is the other
	// half of that rule.
	long := found("ffmpeg", strings.Repeat("9.0-a-very-long-version", 6), deps.SourceFallback)
	long.Path = "/" + strings.Repeat("a-deeply-nested-directory/", 12) + "ffmpeg"

	for _, width := range []int{40, 60, 80, 100, 110} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := &pinned{columns: width}
			if err := runDoctor(context.Background(), out, staticResolver(long),
				noEncoders, "", "", true); err != nil {
				t.Fatalf("runDoctor() err = %v, want nil", err)
			}

			rows := 0
			for line := range strings.SplitSeq(out.String(), "\n") {
				if !strings.ContainsAny(line, glyphOK+glyphBad+glyphWarn+glyphNote) {
					continue
				}
				rows++
				if got := ansi.StringWidth(line); got > width {
					t.Errorf("a %d-column terminal got a %d-column row: %q", width, got, line)
				}
			}
			if rows == 0 {
				t.Fatalf("no rows were rendered, so this proved nothing:\n%s", out.String())
			}
		})
	}
}

func TestRunDoctor_APathOnItsOwnLineIsNeverCut(t *testing.T) {
	// The path in the locations block is the one an operator copies, and a
	// copied path with an ellipsis in the middle goes nowhere. A terminal
	// too narrow for it wraps instead.
	root := filepath.Join(t.TempDir(), strings.Repeat("a-long-directory-name/", 4), "library")
	if _, err := library.Create(root, "test"); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	out := &pinned{columns: 40}
	if err := runDoctor(context.Background(), out, staticResolver(), noEncoders,
		"", root, false); err != nil {
		t.Fatalf("runDoctor() err = %v, want nil", err)
	}

	if !strings.Contains(out.String(), root) {
		t.Errorf("the library root was cut at 40 columns\ngot:\n%s", out.String())
	}
}

func TestRunDoctor_TheRecompressRowIsNeverAPassOrAFailure(t *testing.T) {
	// The code already says this check never fails. A machine with no
	// hardware encoder is a supported machine, so every answer is a note and
	// the mark has to agree with that.
	//
	// Driven with an encoder this machine could run, because that is the
	// one branch where a pass could be claimed. A probe that finds nothing
	// was a note before any of this.
	var out bytes.Buffer
	encoders := staticEncoders(post.Encoder{
		Name: "hevc_nvenc", Codec: config.CodecHEVC, Hardware: true,
	})
	if err := runDoctor(context.Background(), &out, staticResolver(), encoders,
		"", "", false); err != nil {
		t.Fatalf("runDoctor() err = %v, want nil", err)
	}

	seen := false
	for line := range strings.SplitSeq(out.String(), "\n") {
		if !strings.Contains(line, "recompress") {
			continue
		}
		seen = true
		if strings.Contains(line, glyphOK) || strings.Contains(line, glyphBad) {
			t.Errorf("the recompress row claims a pass or a failure: %q", line)
		}
		if !strings.Contains(line, glyphNote) {
			t.Errorf("the recompress row carries no mark at all: %q", line)
		}
	}
	if !seen {
		t.Fatalf("no recompress row was rendered, so this proved nothing:\n%s", out.String())
	}
}

func TestRefuse_PutsTheHintUnderTheCause(t *testing.T) {
	// An error is a result row of one. The half after the semicolon is what
	// to do about it, and it belongs under the cause rather than running on
	// from it.
	var out bytes.Buffer
	refuse(styled(&out), errors.New("a recorder holds this library; stop it first, "+
		"because a second writer would race its sweep"))

	lines := []string{}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) != 2 {
		t.Fatalf("refuse() wrote %d lines, want the cause and the hint under it:\n%s",
			len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], glyphBad) {
		t.Errorf("the cause line = %q, want it to open with the failure mark", lines[0])
	}
	if strings.Contains(lines[0], "stop it first") {
		t.Errorf("the cause line = %q, want the hint on its own line", lines[0])
	}
	if !strings.Contains(lines[1], "stop it first") {
		t.Errorf("the hint line = %q, want what to do about it", lines[1])
	}

	// Indented under the cause rather than merely on a later line. A hint
	// starting in column zero reads as a second, unrelated refusal.
	if !strings.HasPrefix(lines[1], "     ") {
		t.Errorf("the hint line = %q, want it indented under the cause", lines[1])
	}
}

func TestRefuse_AnErrorWithNoHintIsStillARow(t *testing.T) {
	var out bytes.Buffer
	refuse(styled(&out), errors.New("a library path is required"))

	if !strings.Contains(out.String(), glyphBad+"  a library path is required") {
		t.Errorf("refuse() = %q, want the message as a result row", out.String())
	}
}

// Width implements the interface the renderer measures a destination with.
func (p *pinned) Width() int { return p.columns }
