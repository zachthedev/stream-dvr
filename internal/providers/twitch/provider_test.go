package twitch

import (
	"strings"
	"testing"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Provider
// ///////////////////////////////////////////////

func TestProvider_AnswersTheTwitchPlatform(t *testing.T) {
	if got := (Provider{}).Platform(); got != config.PlatformTwitch {
		t.Errorf("Platform() = %q, want %q", got, config.PlatformTwitch)
	}
}

func TestProvider_SeparatesTheLivePageFromTheArchive(t *testing.T) {
	// A recorder pointed at the videos tab captures nothing, and a backfill
	// pointed at the channel page finds no listing. They are one question in
	// two tenses and must not collapse into one answer.
	live := (Provider{}).LiveURL("examplechannel")
	archive := (Provider{}).ArchiveURL("examplechannel")

	if live == archive {
		t.Fatalf("LiveURL and ArchiveURL both answer %q", live)
	}
	if !strings.Contains(archive, "/videos") {
		t.Errorf("ArchiveURL() = %q, want the videos tab", archive)
	}
	if strings.Contains(live, "/videos") {
		t.Errorf("LiveURL() = %q, want the channel page", live)
	}
}

func TestProvider_ArchiveURL_AsksForPastBroadcastsOnly(t *testing.T) {
	// The unfiltered videos tab is the union of archives, highlights and
	// uploads. A highlight carries its source broadcast's timestamp, so it
	// becomes a second broadcast row on a day the streamer did stream, and
	// it consumes one of the listing's slots so a real past broadcast falls
	// out of the window. The API path already asks for archives only, and
	// the two discovery paths must list the same set.
	archive := (Provider{}).ArchiveURL("examplechannel")

	if !strings.Contains(archive, "filter=archives") {
		t.Errorf("ArchiveURL() = %q, want it to ask for past broadcasts only", archive)
	}
}
