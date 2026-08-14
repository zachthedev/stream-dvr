package twitch

import (
	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Provider describes Twitch to the rest of the project.
type Provider struct{}

// ///////////////////////////////////////////////
// Provider
// ///////////////////////////////////////////////

// Platform implements providers.Provider.
func (Provider) Platform() string { return config.PlatformTwitch }

// LiveURL implements providers.Provider.
func (Provider) LiveURL(channel string) string {
	return "https://twitch.tv/" + channel
}

// ArchiveURL implements providers.Provider.
//
// The videos tab lists past broadcasts. It sits beside LiveURL because the
// two answer one question in different tenses. A site given one without the
// other gives a recorder that captures a channel it can never recover.
//
// The filter is what makes it past broadcasts rather than the union of
// archives, highlights and uploads. A highlight carries its source
// broadcast's timestamp, so an unfiltered listing files it as a second
// broadcast on a day the streamer did stream, and it spends one of the
// listing's slots that a real past broadcast needed. The API path asks for
// archives already, and the two discovery paths have to list one set.
func (Provider) ArchiveURL(channel string) string {
	return "https://twitch.tv/" + channel + "/videos?filter=archives&sort=time"
}
