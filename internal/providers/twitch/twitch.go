// Package twitch holds everything specific to Twitch: the recording
// credential, the file streamlink reads it from, the metadata session, and
// the channel URLs.
//
// # Why the token cannot be obtained automatically
//
// streamlink removed official Twitch authentication in 2.0.0. Twitch's
// playback endpoint refuses an access token issued to a third-party client
// id. It accepts only one issued by Twitch's own website, which is the
// auth-token cookie of a signed-in browser session. See
// https://streamlink.github.io/cli/plugins/twitch.html
//
// So the interactive command guides the operator through copying that
// cookie. The manual step is a property of Twitch's API, and the command
// says so plainly.
//
// A token obtained through the device code flow in device.go is a different
// thing with a different use: it works against the Helix REST API for
// metadata, and does not work for playback. Do not offer one where the
// other is wanted.
//
// # What never happens here
//
// No function in this package puts a token in an error, a log line, or a
// process argument. A token reaches a function as a bare string parameter
// and leaves no trace in anything a caller could print.
package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Identity is what Twitch says a token belongs to.
type Identity struct {
	// Login is the account name. It is the operator's real identity, so a
	// caller decides whether to show it rather than printing it by habit.
	Login string
	// ClientID is which application the token was issued to. A browser
	// token names Twitch's own. Anything else will not play a stream.
	ClientID string
	// UserID is the numeric account id.
	UserID string
	// Scopes are the permissions the token carries.
	Scopes []string
	// ExpiresIn is how long the token has left. Zero means Twitch announces
	// no expiry, which is what a browser session token reports. It must not
	// be shown as a countdown that has run out.
	ExpiresIn time.Duration
}

// validateResponse is Twitch's answer to a validation request.
type validateResponse struct {
	ClientID  string   `json:"client_id"`
	Login     string   `json:"login"`
	UserID    string   `json:"user_id"`
	Scopes    []string `json:"scopes"`
	ExpiresIn int      `json:"expires_in"`
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// WebClientID is the application id Twitch's own website issues its session
// tokens under.
//
// The playback endpoint accepts a token from this client and refuses one
// from any other, which is why streamlink dropped Twitch authentication in
// 2.0.0. Measured here: a token issued to a third-party client validates
// cleanly and offers zero streams on a live channel, so validation alone
// cannot tell a working credential from a useless one.
const WebClientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"

// validateURL is the endpoint that says whether a token is still alive.
const validateURL = "https://id.twitch.tv/oauth2/validate"

// DefaultTimeout bounds one request. http.DefaultClient has no timeout at
// all, which would hang an interactive command with no way out but Ctrl-C.
const DefaultTimeout = 30 * time.Second

// maxResponse bounds what is read from a reply.
//
// A validation answer is a few hundred bytes. The bound is what stops a
// wrong endpoint, or a captive portal, from being read into memory without
// limit.
const maxResponse = 64 << 10

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

// ErrInvalidToken reports a token Twitch refused. It is the answer that
// means "ask the operator for a new one", as distinct from a request that
// could not be made at all.
var ErrInvalidToken = errors.New("twitch refused the token")

// ErrTransient reports an answer that is a bad moment rather than a
// position: a rate limit, an outage, anything that clears on its own.
//
// It exists so a caller does not promote one to "the session expired,
// authorize again". A refresh token is spent when it is used, so telling an
// operator to authorize again over a 429 discards a session that was
// working and cannot be undone.
var ErrTransient = errors.New("twitch could not answer just now")

// ///////////////////////////////////////////////
// Validation
// ///////////////////////////////////////////////

// defaultClient returns the client every Twitch call takes when the caller
// names none.
//
// It refuses to follow a redirect. The standard library strips an
// Authorization header when a redirect crosses hosts. It never strips a
// request body, and the token exchange carries the one-time refresh token in
// exactly that. A 307 from the token endpoint would otherwise post the
// credential to whatever host answered. internal/notify makes the same
// choice for the same reason.
//
// The default transport is kept deliberately: it honours HTTP_PROXY and
// HTTPS_PROXY, and a hand-rolled one would silently break an operator
// behind a corporate proxy.
func defaultClient() *http.Client {
	return &http.Client{
		Timeout: DefaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Validate asks Twitch whether a token is still good.
//
// A nil client takes the one defaultClient builds.
//
// The token travels in a header and nowhere else. It is never placed in the
// URL, where it would reach a proxy log and this process's own error
// strings.
func Validate(ctx context.Context, client *http.Client, token string) (Identity, error) {
	if client == nil {
		client = defaultClient()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, validateURL, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("building the validation request: %w", err)
	}
	request.Header.Set("Authorization", "OAuth "+token)

	// G704: the address is validateURL, a package constant. Nothing
	// user-supplied reaches the URL; the token travels in a header.
	response, err := client.Do(request)
	if err != nil {
		// The error from Do names the URL and never the header, so this is
		// safe to wrap. Anything carrying the request itself would not be.
		return Identity{}, fmt.Errorf("asking twitch to validate the token: %w", err)
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusOK:
		return decodeIdentity(response.Body)
	case http.StatusUnauthorized:
		return Identity{}, ErrInvalidToken
	default:
		// Twitch's own words are not repeated: a body from an unexpected
		// status is not known to be free of the request that produced it.
		return Identity{}, fmt.Errorf("twitch answered %d validating the token", response.StatusCode)
	}
}

// decodeIdentity reads a validation body.
func decodeIdentity(body io.Reader) (Identity, error) {
	var decoded validateResponse
	if err := json.NewDecoder(io.LimitReader(body, maxResponse)).Decode(&decoded); err != nil {
		return Identity{}, fmt.Errorf("reading twitch's answer: %w", err)
	}

	return Identity{
		Login:     decoded.Login,
		ClientID:  decoded.ClientID,
		UserID:    decoded.UserID,
		Scopes:    decoded.Scopes,
		ExpiresIn: time.Duration(decoded.ExpiresIn) * time.Second,
	}, nil
}
