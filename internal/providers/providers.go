// Package providers is where every streaming site this project supports is
// described, one subpackage each.
//
// # What belongs in a subpackage
//
// Anything true of one site and no other. That covers the shape of its
// URLs, how it issues credentials, what its listings look like, and how far
// its timestamps can be trusted. A site that changes its URL scheme is one
// directory to edit.
//
// # What belongs here
//
// Only what every site must answer. This package holds the contract and the
// lookup, and imports each subpackage to register it. Nothing here knows
// which site is which.
//
// # Why registration is a written list
//
// Go discovers no subpackage on its own. A provider becomes reachable
// because this file imports it and calls register, so adding a site means
// adding a line here. The set of supported sites is a list a reader can
// see.
package providers

import (
	"fmt"
	"sort"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/providers/youtube"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Provider is what this project needs to know about one streaming site.
//
// A channel name reaches both URL methods, so an implementation must treat
// it as untrusted text. config validates the name before it gets here, and
// neither method can assume more than that.
type Provider interface {
	// Platform is the config value that selects this provider.
	Platform() string
	// LiveURL is where the capture engine is pointed to record now.
	LiveURL(channel string) string
	// ArchiveURL is where past broadcasts are listed for recovery. It
	// answers "" for a site with no listing, which is what stops backfill
	// asking a page that cannot answer.
	ArchiveURL(channel string) string
}

// urlProvider captures whatever streamlink can, from an address the
// operator supplied.
//
// It has no subpackage because it has no site-specific knowledge: the
// channel name is already the URL.
type urlProvider struct{}

// registry maps a platform to its provider.
var registry = map[string]Provider{}

// ///////////////////////////////////////////////
// Registration
// ///////////////////////////////////////////////

// Every supported site is registered here. A provider built but not listed
// is unreachable, and the deadcode linter reports it.
func init() {
	register(twitch.Provider{})
	register(youtube.Provider{})
	register(urlProvider{})
}

// register files a provider under its platform.
//
// It panics on a duplicate because two providers claiming one platform is a
// programming error visible at startup, and resolving it silently would
// make which one wins depend on import order.
func register(provider Provider) {
	if _, taken := registry[provider.Platform()]; taken {
		panic(fmt.Sprintf("two providers claim the platform %q", provider.Platform()))
	}
	registry[provider.Platform()] = provider
}

// ///////////////////////////////////////////////
// Lookup
// ///////////////////////////////////////////////

// For returns the provider a platform selects.
//
// An unknown platform is a configuration this build does not support.
// config validates against SupportedPlatforms before anything gets here, so
// reaching this error means the two lists have drifted apart.
func For(platform string) (Provider, error) {
	if provider, ok := registry[platform]; ok {
		return provider, nil
	}
	return nil, fmt.Errorf("no provider supports the platform %q", platform)
}

// Platforms lists every platform a provider is registered for, sorted.
//
// config keeps its own list of platforms. This one exists so the two can be
// checked against each other rather than trusted to match.
func Platforms() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ///////////////////////////////////////////////
// The bare URL provider
// ///////////////////////////////////////////////

// Platform implements Provider.
func (urlProvider) Platform() string { return config.PlatformURL }

// LiveURL implements Provider. The configured name is the address.
func (urlProvider) LiveURL(channel string) string { return channel }

// ArchiveURL implements Provider.
//
// There is no listing of past broadcasts behind an arbitrary URL, so
// backfill has nothing to search. config refuses backfill on a url channel
// for the same reason.
func (urlProvider) ArchiveURL(string) string { return "" }
