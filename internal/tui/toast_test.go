package tui

import (
	"testing"
	"time"
)

func TestQueueToast_OutcomesAreReadInOrder(t *testing.T) {
	// Each toast starts its life when the one before it ends, so two
	// outcomes in quick succession are both read rather than the first being
	// overwritten unseen.
	model := newModel(t, library(), nil)

	drain(t, model, model.queueToast("first", model.styles.ok))
	drain(t, model, model.queueToast("second", model.styles.ok))

	if len(model.toasts) != 2 {
		t.Fatalf("%d toasts queued, want both", len(model.toasts))
	}
	if !model.toasts[1].Until.After(model.toasts[0].Until) {
		t.Error("the second toast does not outlive the first, so it would never show")
	}
	if got := stripANSI(model.activeToast()); got != "first" {
		t.Errorf("activeToast() = %q, want the one queued first", got)
	}
}

func TestQueueToast_AnEmptyOutcomeIsNotQueued(t *testing.T) {
	model := newModel(t, library(), nil)

	if cmd := model.queueToast("", model.styles.ok); cmd != nil {
		t.Error("an empty toast asked to be woken for")
	}
	if len(model.toasts) != 0 {
		t.Errorf("%d toasts queued for empty text, want none", len(model.toasts))
	}
}

func TestAdvanceToasts_DropsWhatExpired(t *testing.T) {
	model := newModel(t, library(), nil)
	drain(t, model, model.queueToast("gone", model.styles.ok))

	// Past the life of everything queued.
	model.now = func() time.Time { return fixedNow.Add(toastLife * 4) }
	cmd := model.advanceToasts()

	if len(model.toasts) != 0 {
		t.Errorf("%d toasts survived their expiry", len(model.toasts))
	}
	if cmd != nil {
		t.Error("advanceToasts asked to be woken with nothing left to show")
	}
	if got := model.activeToast(); got != "" {
		t.Errorf("activeToast() = %q after everything expired", got)
	}
}

func TestAdvanceToasts_KeepsWhatHasNotExpired(t *testing.T) {
	model := newModel(t, library(), nil)
	drain(t, model, model.queueToast("first", model.styles.ok))
	drain(t, model, model.queueToast("second", model.styles.ok))

	// Past the first toast's life but not the second's.
	model.now = func() time.Time { return fixedNow.Add(toastLife + time.Second) }
	cmd := model.advanceToasts()

	if len(model.toasts) != 1 {
		t.Fatalf("%d toasts left, want the one that has not expired", len(model.toasts))
	}
	if got := stripANSI(model.activeToast()); got != "second" {
		t.Errorf("activeToast() = %q, want the second", got)
	}
	if cmd == nil {
		t.Error("advanceToasts did not ask to be woken for the toast still showing")
	}
}

func TestActiveToast_ANoticeShowsWhereAToastWould(t *testing.T) {
	// Both answer "what just happened" and only one can be true at a time.
	model := newModel(t, library(), nil)
	model.notice = "the recorder in this window stopped"

	if got := stripANSI(model.activeToast()); got != model.notice {
		t.Errorf("activeToast() = %q, want the notice", got)
	}

	drain(t, model, model.queueToast("q quits", model.styles.dim))
	if got := stripANSI(model.activeToast()); got != "q quits" {
		t.Errorf("activeToast() = %q, want the queued toast to outrank the notice", got)
	}
}
