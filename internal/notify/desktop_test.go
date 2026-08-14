package notify

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Rendering
// ///////////////////////////////////////////////

func TestRender_NamesTheEventInTheOperatorsWords(t *testing.T) {
	cases := []struct {
		name      string
		event     Event
		wantTitle string
		wantBody  string
	}{
		{
			name:      "the event that costs a broadcast",
			event:     Event{Kind: "library_full", Channel: "twitch/examplechannel", Detail: "at the cap"},
			wantTitle: "stream-dvr: the library is full",
			wantBody:  "twitch/examplechannel: at the cap",
		},
		{
			name:      "a capture beginning",
			event:     Event{Kind: "recording_started", Channel: "twitch/examplechannel"},
			wantTitle: "stream-dvr: recording started",
			wantBody:  "twitch/examplechannel",
		},
		{
			// The broadcast title stands in when nothing explains the event,
			// because it is the only thing that says which one this was.
			name:      "a failure with only a broadcast title",
			event:     Event{Kind: "failure", Channel: "twitch/examplechannel", Title: "a stream"},
			wantTitle: "stream-dvr: something failed",
			wantBody:  "twitch/examplechannel: a stream",
		},
		{
			// An event kind this build has no wording for still reaches the
			// desktop, rather than arriving blank.
			name:      "a kind added since this list was written",
			event:     Event{Kind: "something_new"},
			wantTitle: "stream-dvr: something_new",
			wantBody:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body := render(tc.event)

			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

func TestRender_EscapesTextAStrangerWrote(t *testing.T) {
	// A stream title is written by whoever was streaming and reaches a
	// surface that renders text. This is the same helper every other human
	// surface in the project goes through.
	title, body := render(Event{
		Kind:    "failure",
		Channel: "twitch/examplechannel",
		Detail:  "went\x1b[2Jaway",
	})

	for _, rendered := range []string{title, body} {
		if strings.ContainsRune(rendered, 0x1b) {
			t.Errorf("%q carries an escape character to the desktop", rendered)
		}
	}
}

func TestRender_ClipsWhatWouldNotFit(t *testing.T) {
	// Every platform truncates somewhere and none of them says where, so
	// one rule here beats three surprises.
	long := strings.Repeat("a stream title ", 60)

	title, body := render(Event{Kind: "failure", Channel: long, Detail: long})

	if got := len([]rune(title)); got > maxTitle {
		t.Errorf("the title is %d runes, want at most %d", got, maxTitle)
	}
	if got := len([]rune(body)); got > maxBody {
		t.Errorf("the body is %d runes, want at most %d", got, maxBody)
	}
	if !strings.HasSuffix(body, "…") {
		t.Errorf("a clipped body does not say it was clipped: %q", body)
	}
}

func TestClip_KeepsWholeRunes(t *testing.T) {
	// Cutting on a byte would split a multi-byte rune and put a replacement
	// character on the operator's screen.
	clipped := clip(strings.Repeat("配信タイトル", 40), 20)

	if got := len([]rune(clipped)); got != 20 {
		t.Errorf("clip() returned %d runes, want 20", got)
	}
	if strings.ContainsRune(clipped, '�') {
		t.Errorf("clip() split a rune: %q", clipped)
	}
}

func TestClip_LeavesTextThatFits(t *testing.T) {
	if got := clip("short", 80); got != "short" {
		t.Errorf("clip() = %q, want the text unchanged", got)
	}
}

// ///////////////////////////////////////////////
// Availability
// ///////////////////////////////////////////////

func TestNewDesktop_AnswersForThisPlatform(t *testing.T) {
	// Refused at construction rather than at the first event, so an
	// operator who turned this on hears about it at startup and not from a
	// broadcast nobody told them about.
	desktop, err := NewDesktop(quiet())

	switch runtime.GOOS {
	case "windows":
		// The recorder is a scheduled task in session 0 and there is no
		// desktop there to post to, so the agent raises one instead.
		if err == nil {
			t.Fatal("NewDesktop() err = nil on Windows, want the agent named as the mechanism")
		}
		if !errors.Is(err, ErrNeedsAgent) {
			t.Errorf("NewDesktop() err = %v, want it to match ErrNeedsAgent", err)
		}
	default:
		// Linux and macOS can both raise one from where the recorder runs,
		// but a build machine has no session bus and no osascript, so a
		// refusal here is a fact about the machine rather than a fault.
		if err != nil {
			t.Logf("no desktop is reachable on this machine: %v", err)
			return
		}
		if desktop == nil {
			t.Error("NewDesktop() returned no sink and no error")
		}
	}
}
