package youtube

import (
	"strings"
	"testing"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Provider
// ///////////////////////////////////////////////

func TestProvider_AnswersTheYouTubePlatform(t *testing.T) {
	if got := (Provider{}).Platform(); got != config.PlatformYouTube {
		t.Errorf("Platform() = %q, want %q", got, config.PlatformYouTube)
	}
}

func TestProvider_ListsCompletedStreamsRatherThanEveryUpload(t *testing.T) {
	// A recording this project missed is a past broadcast. The videos tab
	// would offer every upload, most of which was never a livestream.
	archive := (Provider{}).ArchiveURL("examplechannel")

	if !strings.HasSuffix(archive, "/streams") {
		t.Errorf("ArchiveURL() = %q, want the streams tab", archive)
	}
	if got := (Provider{}).LiveURL("examplechannel"); !strings.HasSuffix(got, "/live") {
		t.Errorf("LiveURL() = %q, want the live tab", got)
	}
}
