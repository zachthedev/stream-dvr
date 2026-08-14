// Package youtube describes YouTube to the rest of the project.
//
// It carries no credential handling. YouTube live capture works from a
// public URL, and the metadata this project reads comes from yt-dlp rather
// than from an authenticated API.
package youtube

import (
	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Provider describes YouTube to the rest of the project.
type Provider struct{}

// ///////////////////////////////////////////////
// Provider
// ///////////////////////////////////////////////

// Platform implements providers.Provider.
func (Provider) Platform() string { return config.PlatformYouTube }

// LiveURL implements providers.Provider.
//
// The live tab of a handle, which resolves to whatever that channel is
// broadcasting now.
func (Provider) LiveURL(channel string) string {
	return "https://www.youtube.com/@" + channel + "/live"
}

// ArchiveURL implements providers.Provider.
//
// The streams tab, which lists completed livestreams rather than every
// upload. A recording this project missed is a past broadcast, not a video.
func (Provider) ArchiveURL(channel string) string {
	return "https://www.youtube.com/@" + channel + "/streams"
}
