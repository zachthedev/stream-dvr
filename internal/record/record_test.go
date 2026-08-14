package record

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// CaptureName
// ///////////////////////////////////////////////

// collidingPrefix is an address two channels can share in full. It is
// longer than the bound on the channel segment, so two addresses built from
// it are identical everywhere the filename can reach.
const collidingPrefix = "https://video.example.com/live/channels/region/eu-west/broadcaster/"

func TestCaptureName(t *testing.T) {
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	// The timestamp comes from the input rather than a literal, so the
	// case asserts the layout and the sanitization instead of arithmetic.
	tests := []struct {
		name     string
		platform string
		channel  string
		wantStem string
	}{
		{
			name:     "twitch channel",
			platform: "twitch",
			channel:  "examplechannel",
			wantStem: "twitch-examplechannel",
		},
		{
			name:     "case is normalized",
			platform: "Twitch",
			channel:  "ExampleChannel",
			wantStem: "twitch-examplechannel",
		},
		{
			name:     "separators in a url source cannot escape the directory",
			platform: "url",
			channel:  "https://example.com/live",
			wantStem: "url-https___example.com_live",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := fmt.Sprintf("%s-%d.%s", tt.wantStem, start.Unix(), CaptureExtension)
			got := CaptureName(tt.platform, tt.channel, start)
			if got != want {
				t.Errorf("CaptureName(%q, %q) = %q, want %q", tt.platform, tt.channel, got, want)
			}
		})
	}
}

func TestCaptureName_NeedsNoMetadata(t *testing.T) {
	// The whole point of this name is that it depends on nothing that can
	// fail. Every input is known before the first byte arrives.
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	got := CaptureName("twitch", "examplechannel", start)
	if got == "" {
		t.Fatal("CaptureName() = empty")
	}
	if !strings.HasSuffix(got, "."+CaptureExtension) {
		t.Errorf("CaptureName() = %q, want the crash-resilient .%s container", got, CaptureExtension)
	}
}

func TestCaptureName_IsUniquePerStart(t *testing.T) {
	base := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	first := CaptureName("twitch", "examplechannel", base)
	second := CaptureName("twitch", "examplechannel", base.Add(time.Second))
	if first == second {
		t.Errorf("CaptureName() returned %q for two different starts", first)
	}
}

func TestCaptureName_BoundsALongChannel(t *testing.T) {
	// A generic URL source has no short name, and the full address would
	// produce a filename no filesystem accepts.
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)
	long := "https://example.com/" + strings.Repeat("segment/", 40)

	got := CaptureName("url", long, start)
	if len([]rune(got)) > maxChannelSegment+40 {
		t.Errorf("CaptureName() = %q (%d runes), want it bounded", got, len([]rune(got)))
	}
	if strings.ContainsAny(got, `/\`) {
		t.Errorf("CaptureName() = %q, want no path separators", got)
	}
}

func TestCaptureName_HandlesAChannelThatSanitizesAway(t *testing.T) {
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	got := CaptureName("twitch", "...", start)
	if got == "" {
		t.Fatal("CaptureName() = empty")
	}
	if strings.HasPrefix(got, "twitch--") {
		t.Errorf("CaptureName() = %q, want a placeholder rather than an empty segment", got)
	}
}

func TestCaptureName_SeparatesChannelsTheNameCannotTellApart(t *testing.T) {
	// Two captures that start in the same second and land on one filename
	// are one file: the second overwrites the first, and the database says
	// both are on disk.
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	tests := []struct {
		name  string
		first string
		other string
	}{
		{
			// A URL source has no short name, so the part that fits is the
			// part every such address shares.
			name:  "two long addresses differing past the bound",
			first: collidingPrefix + "10000",
			other: collidingPrefix + "20000",
		},
		{
			name:  "two names that both sanitize away",
			first: "...",
			other: "   ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := CaptureName("url", tt.first, start)
			other := CaptureName("url", tt.other, start)
			if first == other {
				t.Errorf("CaptureName() = %q for both %q and %q", first, tt.first, tt.other)
			}
		})
	}
}

func TestCaptureName_TruncationAloneCannotSeparateTwoChannels(t *testing.T) {
	// The guard on the case above. Two addresses that already differ inside
	// the bound produce different names whether or not anything carries
	// what truncation dropped, so the case would pass for the wrong reason.
	if len([]rune(collidingPrefix)) <= maxChannelSegment {
		t.Fatalf("the shared prefix is %d runes, want more than the %d-rune bound",
			len([]rune(collidingPrefix)), maxChannelSegment)
	}
}

func TestCaptureName_IsStable(t *testing.T) {
	// The name is derived twice for the same capture, once to write it and
	// once to find it, so it cannot depend on anything but its inputs.
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)
	long := "https://video.example.com/" + strings.Repeat("segment/", 20)

	if first, other := CaptureName("url", long, start), CaptureName("url", long, start); first != other {
		t.Errorf("CaptureName() = %q then %q for the same capture", first, other)
	}
}

// ///////////////////////////////////////////////
// Result
// ///////////////////////////////////////////////

func TestResult_Duration(t *testing.T) {
	start := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	got := Result{StartedAt: start, EndedAt: start.Add(4*time.Hour + 36*time.Minute)}.Duration()
	want := 4*time.Hour + 36*time.Minute
	if got != want {
		t.Errorf("Duration() = %s, want %s", got, want)
	}
}

// ///////////////////////////////////////////////
// CaptureStem
// ///////////////////////////////////////////////

func TestCaptureStem_CarriesNoExtension(t *testing.T) {
	// Backfill appends an extension it does not choose, because a
	// downloaded broadcast arrives as whatever the platform served.
	stem := CaptureStem("twitch", "examplechannel", time.Unix(1772658900, 0).UTC())

	if filepath.Ext(stem) != "" {
		t.Errorf("CaptureStem() = %q, want no extension", stem)
	}
}

func TestCaptureName_IsItsStemPlusTheCaptureExtension(t *testing.T) {
	// One definition of the stem. Two would let the sanitizing drift, and
	// a capture and its backfilled replacement would land on paths that
	// disagree about what a channel name may contain.
	start := time.Unix(1772658900, 0).UTC()

	stem := CaptureStem("twitch", "examplechannel", start)
	name := CaptureName("twitch", "examplechannel", start)

	if want := stem + "." + CaptureExtension; name != want {
		t.Errorf("CaptureName() = %q, want %q", name, want)
	}
}

func TestCaptureStem_SanitizesTheSameWayCaptureNameDoes(t *testing.T) {
	// A channel name that has to be sanitized is exactly where two
	// implementations would part company.
	start := time.Unix(1772658900, 0).UTC()

	for _, channel := range []string{
		"examplechannel",
		"Example Channel",
		"a/channel/with/separators",
		"..",
		"a-very-long-channel-name-that-runs-past-any-sensible-path-component-limit",
	} {
		t.Run(channel, func(t *testing.T) {
			stem := CaptureStem("twitch", channel, start)
			name := CaptureName("twitch", channel, start)

			if want := stem + "." + CaptureExtension; name != want {
				t.Errorf("CaptureName() = %q, want %q", name, want)
			}
		})
	}
}
