package notify

import (
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Nothing here creates a tray. Every call that would is a change to the
// desktop of whoever is running the tests: an icon in the notification
// area, and a balloon over it.

// ///////////////////////////////////////////////
// notifyIconData
// ///////////////////////////////////////////////

func TestNotifyIconData_MatchesTheOperatingSystemLayout(t *testing.T) {
	// Windows reads cbSize to decide which version of the structure it was
	// given, and reads the fields at the offsets that version defines. A
	// structure whose layout has drifted is not rejected: it is read at the
	// wrong offsets, so the balloon text comes out of whatever now sits
	// where Windows expected it.
	var data notifyIconData

	// The size every field above accounts for, as documented for the
	// version carrying a balloon icon.
	const want = 976
	if got := unsafe.Sizeof(data); got != want {
		t.Errorf("sizeof(notifyIconData) = %d, want %d", got, want)
	}

	offsets := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"hWnd", unsafe.Offsetof(data.hWnd), 8},
		{"uID", unsafe.Offsetof(data.uID), 16},
		{"uFlags", unsafe.Offsetof(data.uFlags), 20},
		{"uCallbackMessage", unsafe.Offsetof(data.uCallbackMessage), 24},
		{"hIcon", unsafe.Offsetof(data.hIcon), 32},
		{"szTip", unsafe.Offsetof(data.szTip), 40},
		{"dwState", unsafe.Offsetof(data.dwState), 296},
		{"szInfo", unsafe.Offsetof(data.szInfo), 304},
		{"uVersion", unsafe.Offsetof(data.uVersion), 816},
		{"szInfoTitle", unsafe.Offsetof(data.szInfoTitle), 820},
		{"dwInfoFlags", unsafe.Offsetof(data.dwInfoFlags), 948},
	}
	for _, field := range offsets {
		if field.got != field.want {
			t.Errorf("offsetof(%s) = %d, want %d", field.name, field.got, field.want)
		}
	}
}

func TestNotifyIconData_FieldsHoldAClippedNotification(t *testing.T) {
	// The clip is what an operator is promised, and a field smaller than it
	// takes a second cut nobody stated. Counted in UTF-16 units, because
	// that is what these fields hold.
	var data notifyIconData

	// A stream title carries whatever the streamer typed, so every rune of
	// a body may be a surrogate pair costing two units.
	if want := maxBody*2 + 1; len(data.szInfo) < want {
		t.Errorf("szInfo holds %d units, want at least %d for a %d-rune body and a terminator",
			len(data.szInfo), want, maxBody)
	}

	// The title is composed here from a fixed set of English phrases, so a
	// rune is one unit.
	if want := maxTitle + 1; len(data.szInfoTitle) < want {
		t.Errorf("szInfoTitle holds %d units, want at least %d for a %d-rune title and a terminator",
			len(data.szInfoTitle), want, maxTitle)
	}
}

func TestSummarize_StaysWithinTheTitleField(t *testing.T) {
	// The title's budget rests on every rendered title being ASCII and
	// short. A phrase added to summarize is where that stops being true.
	for _, kind := range []string{
		"recording_started", "failure", "library_full", "downtime",
	} {
		title, _ := render(Event{Kind: kind})
		for _, r := range title {
			if r > 127 {
				t.Errorf("the title for %q carries %q, which is not one UTF-16 unit", kind, r)
			}
		}
		if len(title) > maxTitle {
			t.Errorf("the title for %q is %d runes, over the %d-rune bound", kind, len(title), maxTitle)
		}
	}
}

// ///////////////////////////////////////////////
// copyUTF16
// ///////////////////////////////////////////////

func TestCopyUTF16(t *testing.T) {
	tests := []struct {
		name  string
		field int
		text  string
		want  string
		why   string
	}{
		{
			name:  "writes the text",
			field: 16,
			text:  "recording started",
			want:  "recording started"[:15],
			why:   "seventeen characters do not fit fifteen and a terminator",
		},
		{
			name:  "leaves room for the terminator",
			field: 8,
			text:  "abcdefghijkl",
			want:  "abcdefg",
			why:   "Windows reads to a NUL, so the last unit can never be text",
		},
		{
			name:  "keeps a short string whole",
			field: 32,
			text:  "examplechannel",
			want:  "examplechannel",
			why:   "text well inside the field is not touched",
		},
		{
			name:  "handles an empty string",
			field: 8,
			text:  "",
			want:  "",
			why:   "an event with no detail renders an empty body",
		},
		{
			name:  "carries text outside the basic plane",
			field: 32,
			text:  "a 🎥 broadcast",
			want:  "a 🎥 broadcast",
			why:   "a stream title is whatever the streamer typed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := make([]uint16, tt.field)
			copyUTF16(field, tt.text)

			if got := decodeUTF16(field); got != tt.want {
				t.Errorf("copyUTF16(%q) = %q, want %q (%s)", tt.text, got, tt.want, tt.why)
			}
			if field[len(field)-1] != 0 {
				t.Errorf("copyUTF16(%q) left no terminator in the last unit", tt.text)
			}
		})
	}
}

func TestCopyUTF16_DoesNotRunPastTheField(t *testing.T) {
	// A write past the end is a write into whichever field follows in the
	// structure, which Windows then reads as a path or a handle.
	const size = 8
	backing := make([]uint16, size*2)
	for i := range backing {
		backing[i] = 0xFFFF
	}

	copyUTF16(backing[:size], strings.Repeat("x", 100))

	for i := size; i < len(backing); i++ {
		if backing[i] != 0xFFFF {
			t.Fatalf("copyUTF16 wrote to unit %d, past the %d-unit field", i, size)
		}
	}
}

// decodeUTF16 reads a NUL-terminated field back as a string.
func decodeUTF16(field []uint16) string {
	end := len(field)
	for i, unit := range field {
		if unit == 0 {
			end = i
			break
		}
	}
	return windows.UTF16ToString(field[:end])
}

// ///////////////////////////////////////////////
// ownsConsole
// ///////////////////////////////////////////////

// hideConsole itself is not called here, for the reason nothing else in this
// file is called: it changes the desktop of whoever is running the tests,
// and getting it wrong takes their shell off the screen with no way back.
// ownsConsole is the answer it acts on, and testing that is what makes the
// call above it safe.

func TestOwnsConsole_AgreesWithHowManyAreAttached(t *testing.T) {
	// The two-slot buffer is the subtle part. GetConsoleProcessList has to
	// report the true count even when the buffer could not hold it, or a
	// console shared with a shell reads as one this process owns alone and
	// hideConsole takes that shell down.
	var attached [64]uint32
	count, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&attached[0])), uintptr(len(attached)))
	if count == 0 {
		t.Skip("this process has no console to ask about")
	}

	if got, want := ownsConsole(), count == 1; got != want {
		t.Errorf("ownsConsole() = %v with %d processes attached, want %v", got, count, want)
	}
}

func TestOwnsConsole_SaysNoWhenAShellIsSharingTheConsole(t *testing.T) {
	// The case the operator hits: a test run, and an operator running the
	// agent by hand. Both are attached to a console their shell already
	// holds, and neither may hide it.
	var attached [64]uint32
	count, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&attached[0])), uintptr(len(attached)))
	if count <= 1 {
		t.Skip("this test binary has a console to itself, so nothing is sharing it")
	}

	if ownsConsole() {
		t.Errorf("ownsConsole() = true with %d processes attached, want false", count)
	}
}
