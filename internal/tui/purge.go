package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/retention"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Purge is what the model needs to offer a purge and carry one out.
//
// Ranking happens outside the model because the weights are configuration.
// Trashing happens outside it because the move takes the organizer's
// per-recording lock, which the sweep holds while it retries the same rows.
type Purge interface {
	// Candidates ranks what may be purged, cheapest to lose first.
	Candidates() ([]retention.Candidate, error)
	// Trash moves one recording out of the library. It frees nothing: the
	// bytes stay counted until the grace expires and the release runs.
	Trash(recordingID int64) error
}

// candidatesMsg carries a completed ranking.
type candidatesMsg struct {
	candidates []retention.Candidate
	err        error
}

// purgedMsg carries the result of a confirmed purge.
//
// The count and the error are both present because a purge is many moves
// and some can fail while others succeed. Reporting only the failure would
// hide that anything happened at all.
type purgedMsg struct {
	purged int
	err    error
}

// ///////////////////////////////////////////////
// Opening and keys
// ///////////////////////////////////////////////

// openPurge focuses the purge pane and asks for a fresh ranking.
//
// The ranking is never cached across a visit. It is a snapshot of a library
// the recorder is still writing to, and an operator returning to a stale
// list would be choosing from rows that have since been finalized, pinned,
// or purged.
func (m *Model) openPurge() tea.Cmd {
	if m.purge == nil {
		m.err = errors.New("this library cannot be purged from here")
		return nil
	}

	m.pane = panePurge
	m.candidate = 0
	m.confirming = false
	m.purged, m.purgeErr = 0, nil
	m.selected = make(map[int64]bool)
	return m.loadCandidates()
}

// handlePurgeKey applies a key press with the purge pane focused.
func (m *Model) handlePurgeKey(key string) tea.Cmd {
	// While a confirmation stands, only the two keys that answer it are
	// live. Moving the cursor or changing the selection under a prompt
	// naming a count would leave the prompt describing a different set.
	if m.confirming {
		switch key {
		case "enter":
			// Refused while one is already in flight. The answer only
			// clears when it comes back, so key autorepeat on enter would
			// otherwise turn a gate that costs two deliberate presses into
			// one trash attempt per repeat, against the same selection, on
			// the only destructive path in the application.
			if m.purging {
				return nil
			}
			return m.performPurge()
		case "esc":
			m.confirming = false
		}
		return nil
	}

	switch key {
	case "esc":
		m.pane = paneCalendar
		return nil

	case "up", "k":
		m.moveCandidate(-1)
	case "down", "j":
		m.moveCandidate(1)

	// The space bar names itself rather than reporting the character it
	// produced, which is the one printable key a terminal reports by name.
	case "space":
		m.toggleSelected()

	case "enter":
		// Nothing is deleted by the key that opens the prompt. This is the
		// only destructive path in the application, and it costs two
		// deliberate presses rather than one.
		if len(m.selected) > 0 {
			m.confirming = true
		}

	case "r":
		return m.loadCandidates()
	}
	return nil
}

// moveCandidate walks the ranking, stopping at each end.
func (m *Model) moveCandidate(step int) {
	m.candidate = min(max(m.candidate+step, 0), max(len(m.candidates)-1, 0))
}

// toggleSelected adds or removes the candidate under the cursor.
func (m *Model) toggleSelected() {
	candidate, ok := m.selectedCandidate()
	if !ok {
		return
	}

	id := candidate.Recording.ID
	if m.selected[id] {
		delete(m.selected, id)
		return
	}
	m.selected[id] = true
}

// selectedCandidate returns the candidate under the cursor.
func (m *Model) selectedCandidate() (retention.Candidate, bool) {
	if m.candidate < 0 || m.candidate >= len(m.candidates) {
		return retention.Candidate{}, false
	}
	return m.candidates[m.candidate], true
}

// chosen returns the candidates the operator selected, in ranked order.
func (m *Model) chosen() []retention.Candidate {
	var chosen []retention.Candidate

	for _, candidate := range m.candidates {
		if m.selected[candidate.Recording.ID] {
			chosen = append(chosen, candidate)
		}
	}
	return chosen
}

// ///////////////////////////////////////////////
// Commands
// ///////////////////////////////////////////////

// loadCandidates ranks the library off the event loop.
func (m *Model) loadCandidates() tea.Cmd {
	purge := m.purge

	return func() tea.Msg {
		candidates, err := purge.Candidates()
		return candidatesMsg{candidates: candidates, err: err}
	}
}

// performPurge trashes every selected recording.
//
// Each move is attempted even after one fails. A purge is the operator's
// answer to a full library, and stopping at the first row the organizer
// happens to hold would free almost nothing and report a failure.
func (m *Model) performPurge() tea.Cmd {
	purge := m.purge
	chosen := m.chosen()
	m.purging = true

	return func() tea.Msg {
		msg := purgedMsg{}

		var failures []error
		for _, candidate := range chosen {
			if err := purge.Trash(candidate.Recording.ID); err != nil {
				failures = append(failures, err)
				continue
			}
			msg.purged++
		}

		msg.err = errors.Join(failures...)
		return msg
	}
}

// applyCandidates folds a completed ranking into the model.
func (m *Model) applyCandidates(msg candidatesMsg) tea.Cmd {
	m.err = msg.err
	if msg.err != nil {
		return nil
	}

	m.candidates = msg.candidates
	m.candidate = min(m.candidate, max(len(m.candidates)-1, 0))

	// A selection is dropped when the ranking behind it changes, because
	// the rows it named may no longer be on the list at all.
	m.selected = make(map[int64]bool)
	return nil
}

// applyPurged folds a completed purge into the model.
//
// It reloads both the ranking and the calendar. The purged rows are gone
// from one and marked trashed on the other, and a screen still showing them
// is what makes an operator purge the same recording twice.
func (m *Model) applyPurged(msg purgedMsg) tea.Cmd {
	m.purging = false
	m.confirming = false
	m.purged, m.purgeErr = msg.purged, msg.err
	return tea.Batch(m.loadCandidates(), m.refresh())
}

// ///////////////////////////////////////////////
// View
// ///////////////////////////////////////////////

// purgeView draws the ranking, the running total, and any prompt.
func (m *Model) purgeView() (head, rows, tail []string, at int) {
	head = []string{m.styles.dim.Render("cheapest to lose first"), ""}
	if m.err != nil {
		head = append(head, m.styles.err.Render(escape.Text(m.err.Error())), "")
	}

	if len(m.candidates) == 0 {
		rows = append(rows, m.styles.dim.Render("nothing may be purged"))
	}
	for i, candidate := range m.candidates {
		rows = append(rows, splitLines(m.candidateLine(i, candidate))...)
	}

	// The summary is drawn even over an empty ranking. Purging everything
	// empties the list, and a pane that dropped the summary at that point
	// would answer the operator's only destructive action with silence.
	if summary := m.purgeSummary(); summary != "" {
		tail = append(tail, "")
		tail = append(tail, splitLines(summary)...)
	}

	// Each candidate takes two rows, so the cursor is at twice its index.
	return head, rows, tail, m.candidate * 2
}

// candidateLine renders one row of the ranking with the reasons it scored.
func (m *Model) candidateLine(i int, candidate retention.Candidate) string {
	cursor := "  "
	if i == m.candidate {
		cursor = m.styles.heading.Render("> ")
	}

	box := "[ ]"
	if m.selected[candidate.Recording.ID] {
		box = "[x]"
	}

	return fmt.Sprintf("%s%s %s  %s\n%s\n", cursor, box,
		config.Size(candidate.Recording.Bytes),
		escape.Text(candidate.Recording.Path),
		m.styles.dim.Render("        "+reasonsFor(candidate)))
}

// purgeSummary reports what the selection would move, asks to confirm it,
// or reports what the last purge did.
func (m *Model) purgeSummary() string {
	chosen := m.chosen()

	if m.confirming {
		// The prompt says move rather than delete, because that is what
		// happens. The bytes come back when the grace expires, and saying
		// otherwise would make the next screen read as a failure.
		return m.styles.err.Render(fmt.Sprintf(
			"move %d %s (%s) to the trash? enter confirms, esc cancels",
			len(chosen), plural(len(chosen), "recording", "recordings"),
			config.Size(retention.Bytes(chosen))))
	}

	if len(chosen) > 0 {
		return m.styles.dim.Render(fmt.Sprintf("%d %s selected, %s", len(chosen),
			plural(len(chosen), "recording", "recordings"),
			config.Size(retention.Bytes(chosen))))
	}
	return m.purgeOutcome()
}

// purgeOutcome reports what the last purge did.
//
// It is a separate field from the model's error because a purge is always
// followed by a reload, and a ranking that succeeds afterward would
// otherwise clear the report of a move that failed.
func (m *Model) purgeOutcome() string {
	moved := fmt.Sprintf("%d %s moved to the trash", m.purged,
		plural(m.purged, "recording", "recordings"))

	switch {
	case m.purgeErr != nil && m.purged > 0:
		return m.styles.err.Render(moved + ", and " + escape.Text(m.purgeErr.Error()))
	case m.purgeErr != nil:
		return m.styles.err.Render("nothing moved: " + escape.Text(m.purgeErr.Error()))
	case m.purged > 0:
		return m.styles.dim.Render(moved)
	case len(m.candidates) > 0:
		return m.styles.dim.Render("nothing selected")
	default:
		return ""
	}
}

// reasonsFor renders why a candidate scored where it did.
func reasonsFor(candidate retention.Candidate) string {
	if len(candidate.Reasons) == 0 {
		return "no reason scored"
	}

	whys := make([]string, 0, len(candidate.Reasons))
	for _, reason := range candidate.Reasons {
		whys = append(whys, reason.Why)
	}
	return strings.Join(whys, ", ")
}
