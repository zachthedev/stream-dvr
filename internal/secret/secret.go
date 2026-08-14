// Package secret keeps the credentials this project was given.
//
// # Who reads a store
//
// Both an interactive command and the daemon. A platform credential manager
// cannot serve both. A rotating credential must be written back by whoever
// refreshes it, and the recorder runs with no interactive session to reach a
// keychain through.
//
// # What a store is not
//
// It is not encryption. A secret here is protected by the file mode and by
// the data directory's permissions. That is the same boundary config.toml
// sits behind, and config.toml already carries a webhook URL that is itself
// a credential.
//
// This package avoids the platform credential managers. streamlink can only
// read a credential from a file, so a readable copy exists for as long as
// the recorder runs, whatever else holds the credential. A store beside it
// would protect nothing while implying that it did. See File for the whole
// argument.
package secret

import (
	"errors"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Store keeps one secret per account under a fixed service name.
//
// Get answers ErrNotFound for an account nothing was stored under. That is
// the ordinary state before the operator authenticates, and it must not read
// as a failure.
type Store interface {
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
}

// Memory is a Store held in this process and nowhere else.
//
// It is exported because every other package's tests use it as a fake. It is
// also the right store for a run that must leave nothing behind.
type Memory struct {
	values map[string]string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// AccountTwitch names the Twitch credential the recorder captures with.
const AccountTwitch = "twitch"

// AccountTwitchAPI names the Twitch metadata session.
//
// Kept apart from AccountTwitch because the two are not interchangeable.
// Swapping them takes the recorder offline. Twitch's playback endpoint
// refuses a token issued through the device flow: measured against a live
// channel, that token offered zero streams where the browser credential
// played every quality. One account holding both would make that swap an
// ordinary mistake.
const AccountTwitchAPI = "twitch-api"

// maxSecretBytes bounds what a store accepts.
//
// A Twitch token is about thirty characters and a refresh pair not much
// more, so nothing legitimate comes close. The bound is what stops a
// caller writing a whole response body in by mistake.
const maxSecretBytes = 2048

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

// ErrNotFound reports an account nothing is stored under.
var ErrNotFound = errors.New("no secret is stored for that account")

// ErrTooLarge reports a secret past what a store accepts.
var ErrTooLarge = errors.New("the secret is larger than a credential store accepts")

// ///////////////////////////////////////////////
// Memory
// ///////////////////////////////////////////////

// NewMemory returns an empty in-process store.
func NewMemory() *Memory { return &Memory{values: map[string]string{}} }

// Get implements Store.
func (m *Memory) Get(account string) (string, error) {
	if value, ok := m.values[account]; ok {
		return value, nil
	}
	return "", ErrNotFound
}

// Set implements Store.
func (m *Memory) Set(account, secret string) error {
	if err := checkSize(secret); err != nil {
		return err
	}
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[account] = secret
	return nil
}

// Delete implements Store.
//
// Removing what is not there is the desired end state rather than a
// failure, which is what lets a logout run twice.
func (m *Memory) Delete(account string) error {
	delete(m.values, account)
	return nil
}

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// checkSize refuses a secret larger than a store accepts.
//
// The error names neither the secret nor its length, because an error is
// the one place a credential reliably escapes: it gets wrapped, logged, and
// pasted into a bug report.
func checkSize(secret string) error {
	if len(secret) > maxSecretBytes {
		return ErrTooLarge
	}
	return nil
}
