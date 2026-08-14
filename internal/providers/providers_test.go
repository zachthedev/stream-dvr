package providers

import (
	"slices"
	"testing"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// The registry
// ///////////////////////////////////////////////

func TestPlatforms_CoverEveryPlatformConfigAccepts(t *testing.T) {
	// The one drift this layout can suffer. config validates a channel
	// against SupportedPlatforms, so a platform it accepts with no provider
	// behind it reaches the daemon and finds nothing to point streamlink at.
	registered := Platforms()

	for _, platform := range config.SupportedPlatforms {
		if !slices.Contains(registered, platform) {
			t.Errorf("config accepts %q with no provider registered for it", platform)
		}
	}
	for _, platform := range registered {
		if !slices.Contains(config.SupportedPlatforms, platform) {
			t.Errorf("a provider is registered for %q, which config refuses", platform)
		}
	}
}

func TestFor_ReportsAPlatformNothingSupports(t *testing.T) {
	// Reaching this means the two lists above have drifted. It answers an
	// error rather than a nil provider, because a nil would panic on the
	// recording path and not at the lookup.
	if _, err := For("a-site-this-build-never-heard-of"); err == nil {
		t.Error("For() err = nil for an unknown platform, want a failure")
	}
}

// ///////////////////////////////////////////////
// What each provider answers
// ///////////////////////////////////////////////

func TestFor_ReturnsAProviderThatBuildsBothURLs(t *testing.T) {
	tests := []struct {
		platform   string
		channel    string
		wantLive   string
		wantArchiv string
	}{
		{
			platform: config.PlatformTwitch,
			channel:  "examplechannel",
			wantLive: "https://twitch.tv/examplechannel",
			// Filtered to past broadcasts, so the listing path and the API
			// path describe the same set rather than the listing also
			// carrying highlights and uploads.
			wantArchiv: "https://twitch.tv/examplechannel/videos?filter=archives&sort=time",
		},
		{
			platform:   config.PlatformYouTube,
			channel:    "examplechannel",
			wantLive:   "https://www.youtube.com/@examplechannel/live",
			wantArchiv: "https://www.youtube.com/@examplechannel/streams",
		},
		{
			platform: config.PlatformURL,
			channel:  "https://example.com/stream.m3u8",
			wantLive: "https://example.com/stream.m3u8",
			// No listing exists behind an arbitrary address.
			wantArchiv: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			provider, err := For(tt.platform)
			if err != nil {
				t.Fatalf("For(%q) err = %v, want nil", tt.platform, err)
			}
			if got := provider.LiveURL(tt.channel); got != tt.wantLive {
				t.Errorf("LiveURL() = %q, want %q", got, tt.wantLive)
			}
			if got := provider.ArchiveURL(tt.channel); got != tt.wantArchiv {
				t.Errorf("ArchiveURL() = %q, want %q", got, tt.wantArchiv)
			}
		})
	}
}

func TestFor_ReturnsAProviderFiledUnderItsOwnPlatform(t *testing.T) {
	// The registry keys on this, so a provider that answered someone else's
	// platform would be filed under it and silently serve the wrong site.
	for _, platform := range Platforms() {
		provider, err := For(platform)
		if err != nil {
			t.Fatalf("For(%q) err = %v, want nil", platform, err)
		}
		if got := provider.Platform(); got != platform {
			t.Errorf("provider filed under %q reports %q", platform, got)
		}
	}
}
