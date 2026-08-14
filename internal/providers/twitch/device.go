package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// DeviceCode is what Twitch hands back to start a device flow.
type DeviceCode struct {
	// DeviceCode identifies this attempt when polling. It is a credential
	// for the duration of the flow and is never printed.
	DeviceCode string
	// UserCode is what the operator types into the page. It is meant to be
	// read aloud and shown.
	UserCode string
	// VerificationURI is where they enter it. It is shown as the server
	// gave it, not as a constant here. Twitch is free to move the page, and
	// an operator sent to the wrong one is stuck.
	VerificationURI string
	// ExpiresIn is how long the code is good for.
	ExpiresIn time.Duration
	// Interval is how often Twitch is willing to be polled. Honour it
	// rather than choosing: polling faster earns a slow_down and then a
	// refusal.
	Interval time.Duration
	// CodeIsInTheURI reports whether VerificationURI already carries the
	// user code, so the caller knows whether to state it separately.
	//
	// Recorded where the page is chosen rather than worked out afterwards
	// from the two strings. RFC 8628's verification_uri_complete is the
	// signal, and asking whether one string contains the other is not a
	// substitute for it: a short code, or one that happens to appear in a
	// host or a query parameter, answers yes about a page that shows the
	// operator nothing.
	CodeIsInTheURI bool
}

// Tokens is a granted pair.
//
// A refresh token is ONE TIME USE. The caller must store the new pair before
// discarding the one it passed in. A crash between a successful refresh and
// the write loses the session for good, and the operator authorizes again.
type Tokens struct {
	Access  string
	Refresh string
	// ExpiresIn is how long the access token has. Unlike a browser session
	// token, a device-flow one really does expire.
	ExpiresIn time.Duration
}

// pollState is what one poll reports.
type pollState int

// deviceResponse is Twitch's answer to a device request.
type deviceResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	// VerificationURIComplete is RFC 8628's optional page that already
	// carries the code. Twitch sends no such field and puts the code in
	// verification_uri instead, so this is here for a provider that follows
	// the specification rather than for the one in hand.
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// tokenResponse is Twitch's answer to a token request, granted or not.
type tokenResponse struct {
	// G117: this is the wire shape of Twitch's own reply. The
	// struct is unexported, never logged, and never re-serialised; the
	// fields hold tokens because that is what the endpoint returns.
	AccessToken string `json:"access_token"`
	// G117: same reply, same reasoning as the field above.
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	// Message carries the pending, slow_down, and refusal states. Twitch
	// reports them here rather than through distinct status codes.
	Message string `json:"message"`
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// The states one poll can report.
const (
	statePending pollState = iota
	stateSlowDown
	stateGranted
)

// The device flow endpoints.
const (
	deviceURL = "https://id.twitch.tv/oauth2/device"
	//nolint:gosec // G101: an endpoint address, not a credential.
	tokenURL = "https://id.twitch.tv/oauth2/token"
)

// Bounds on the timings a device response carries.
//
// Every one of these is remote text, and both fields are multiplied into a
// Duration, so a value the provider never meant still has to land somewhere
// this client can act on. RFC 8628 suggests five seconds between polls; the
// range around it is wide enough for any provider and narrow enough that
// nothing degenerates.
const (
	minPollSeconds   = 1
	maxPollSeconds   = 300
	minExpirySeconds = 1
	maxExpirySeconds = 3600
)

// deviceGrant is the grant type RFC 8628 defines for this flow.
//
// G101: the grant type RFC 8628 defines. A public constant.
const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// slowDownStep is what RFC 8628 says to add to the interval each time the
// server asks for a slower poll.
const slowDownStep = 5 * time.Second

// noScopes is what this flow asks for.
//
// Helix Get Videos and Get Users are everything this project reaches with a
// device token, and both accept any valid user token without a scope. Asking
// for one the code never exercises is requesting account access with no
// reason to show the operator. Add one only when an endpoint demands it, and
// record which endpoint here.
const noScopes = ""

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

var (
	// ErrNoClientID reports an install that names no Twitch application.
	// The device flow cannot start without one. A browser token needs
	// none, so this is not fatal to the project.
	ErrNoClientID = errors.New("no twitch client id is configured")

	// ErrDeviceCodeExpired reports a code the operator did not enter in
	// time. Starting again is the whole remedy.
	ErrDeviceCodeExpired = errors.New("the device code expired before it was authorized")

	// ErrAuthorizationDenied reports an operator who refused at the page.
	ErrAuthorizationDenied = errors.New("authorization was refused")
)

// ///////////////////////////////////////////////
// Starting a flow
// ///////////////////////////////////////////////

// StartDevice asks Twitch for a code the operator can authorize.
//
// The client id names the Twitch application this install acts as, and it
// comes from the operator's config. Every install registers its own, so a
// downloaded build carries no tie to whoever produced it and spends no
// rate limit that is not the operator's own.
func StartDevice(ctx context.Context, client *http.Client, clientID string) (DeviceCode, error) {
	if clientID == "" {
		return DeviceCode{}, ErrNoClientID
	}

	body, err := postForm(ctx, client, deviceURL, url.Values{
		"client_id": {clientID},
		"scopes":    {noScopes},
	})
	if err != nil {
		return DeviceCode{}, err
	}

	var decoded deviceResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return DeviceCode{}, fmt.Errorf("reading the device response: %w", err)
	}
	if decoded.DeviceCode == "" || decoded.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("twitch returned no device code")
	}

	// The page that already carries the code wins where one is offered,
	// because it is the one an operator can open and be done with.
	page := decoded.VerificationURI
	carries := decoded.VerificationURIComplete != ""
	if carries {
		page = decoded.VerificationURIComplete
	} else {
		carries = pageCarriesCode(page, decoded.UserCode)
	}

	return DeviceCode{
		DeviceCode:      decoded.DeviceCode,
		UserCode:        decoded.UserCode,
		VerificationURI: page,
		CodeIsInTheURI:  carries,
		ExpiresIn:       clampExpiry(decoded.ExpiresIn),
		Interval:        clampInterval(decoded.Interval),
	}, nil
}

// pageCarriesCode reports whether a verification page already presents the
// user code, for a provider that fills the page without sending RFC 8628's
// verification_uri_complete.
//
// The comparison is structural: a query parameter whose whole value is the
// code. Asking only whether the address contains the code somewhere answers
// yes for a code short enough to occur by chance, for one that appears in
// the host, and for one sitting in a fragment the browser never sends, and
// each wrong yes withholds a code the operator needs.
func pageCarriesCode(page, code string) bool {
	if code == "" {
		return false
	}

	parsed, err := url.Parse(page)
	if err != nil {
		return false
	}
	for _, values := range parsed.Query() {
		if slices.Contains(values, code) {
			return true
		}
	}
	return false
}

// refusesTheGrant reports a refusal that is about the refresh token rather
// than about the moment it was asked.
//
// An empty message is treated as a refusal of the grant: an answer with no
// access token and nothing to say is the shape a spent token produces, and
// reading it as transient would retry a credential that can never work.
func refusesTheGrant(message string) bool {
	lowered := strings.ToLower(message)
	if lowered == "" {
		return true
	}
	for _, phrase := range []string{"invalid_grant", "invalid refresh", "invalid token", "expired", "revoked"} {
		if strings.Contains(lowered, phrase) {
			return true
		}
	}
	return false
}

// clampInterval turns a remote poll interval into one this client will
// honour.
//
// Seconds multiplied into nanoseconds overflows int64 well inside what the
// field can carry, and a Duration that wraps negative makes every timer
// fire at once: the flow then polls the provider's token endpoint as fast
// as the machine allows, for as long as the code is valid, which is how an
// address gets rate limited and then refused.
func clampInterval(seconds int) time.Duration {
	return time.Duration(min(max(seconds, minPollSeconds), maxPollSeconds)) * time.Second
}

// clampExpiry turns a remote lifetime into one this client will honour,
// bounded for the same reason clampInterval is.
//
// The floor matters as much as the ceiling: a lifetime at or before now
// makes every token read run the exchange again, and each one spends a
// credential that can only be spent once.
func clampExpiry(seconds int) time.Duration {
	return time.Duration(min(max(seconds, minExpirySeconds), maxExpirySeconds)) * time.Second
}

// ///////////////////////////////////////////////
// Polling
// ///////////////////////////////////////////////

// PollDevice waits until the operator authorizes the code, or it expires.
//
// It polls at the interval the SERVER asked for and widens it by
// slowDownStep whenever Twitch says to, per RFC 8628. Choosing an interval
// here instead would earn a refusal from a server that already said how
// often it wants to be asked.
//
// Cancellation is honoured between polls, so Ctrl-C ends this at once
// rather than at the next tick.
func PollDevice(ctx context.Context, client *http.Client, code DeviceCode, clientID string) (Tokens, error) {
	interval := code.Interval
	deadline := time.Now().Add(code.ExpiresIn)

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return Tokens{}, ctx.Err()
		case <-timer.C:
		}

		tokens, state, err := pollOnce(ctx, client, code.DeviceCode, clientID)
		switch {
		case err != nil:
			return Tokens{}, err
		case state == statePending:
		case state == stateSlowDown:
			// The server is asking to be left alone longer. Widening rather
			// than holding is what stops the next poll being refused.
			interval += slowDownStep
		case state == stateGranted:
			return tokens, nil
		}

		if !time.Now().Before(deadline) {
			return Tokens{}, ErrDeviceCodeExpired
		}
		timer.Reset(interval)
	}
}

// pollOnce asks once whether the code has been authorized.
func pollOnce(ctx context.Context, client *http.Client, deviceCode, clientID string) (Tokens, pollState, error) {
	body, err := postForm(ctx, client, tokenURL, url.Values{
		"client_id":   {clientID},
		"scopes":      {noScopes},
		"device_code": {deviceCode},
		"grant_type":  {deviceGrant},
	})
	if err != nil {
		return Tokens{}, statePending, err
	}

	var decoded tokenResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Tokens{}, statePending, fmt.Errorf("reading the token response: %w", err)
	}

	if decoded.AccessToken != "" {
		return Tokens{
			Access:    decoded.AccessToken,
			Refresh:   decoded.RefreshToken,
			ExpiresIn: time.Duration(decoded.ExpiresIn) * time.Second,
		}, stateGranted, nil
	}

	switch message := strings.ToLower(decoded.Message); {
	case strings.Contains(message, "authorization_pending"):
		return Tokens{}, statePending, nil
	case strings.Contains(message, "slow_down"):
		return Tokens{}, stateSlowDown, nil
	case strings.Contains(message, "expired"):
		return Tokens{}, statePending, ErrDeviceCodeExpired
	case strings.Contains(message, "denied"), strings.Contains(message, "declin"):
		return Tokens{}, statePending, ErrAuthorizationDenied
	default:
		// Twitch's own words, which carry no credential: this response is a
		// refusal, so it holds no token to leak.
		return Tokens{}, statePending, fmt.Errorf("twitch refused the device flow: %s", decoded.Message)
	}
}

// ///////////////////////////////////////////////
// Refresh
// ///////////////////////////////////////////////

// Refresh exchanges a refresh token for a new pair.
//
// THE CALLER MUST STORE THE RESULT BEFORE DISCARDING WHAT IT PASSED IN.
// Twitch refresh tokens are one time use, so the token handed here is dead
// the moment this succeeds. A crash before the write leaves the session
// unrecoverable.
func Refresh(ctx context.Context, client *http.Client, refreshToken, clientID string) (Tokens, error) {
	if clientID == "" {
		return Tokens{}, ErrNoClientID
	}

	// the body is read below whether or not the status was
	// a transient refusal, and the error is carried forward with it.
	body, err := postForm(ctx, client, tokenURL, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return Tokens{}, err
	}

	var decoded tokenResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Tokens{}, fmt.Errorf("reading the refresh response: %w", err)
	}
	if decoded.AccessToken == "" {
		// Only an answer naming the grant itself means the refresh token
		// is spent or revoked. Everything else is the endpoint having a
		// bad moment, and promoting one of those to "authorize again"
		// throws away a session that was working: the operator is told to
		// sign in again over a rate limit that clears on its own.
		//
		// Matched on Twitch's own words as well as the OAuth code, the way
		// the poll states above are, because the endpoint answers
		// "Invalid refresh token" rather than "invalid_grant".
		if !refusesTheGrant(decoded.Message) {
			return Tokens{}, fmt.Errorf("%w: twitch refused the refresh", ErrTransient)
		}
		return Tokens{}, ErrInvalidToken
	}

	// RFC 6749 section 5.1 makes refresh_token optional in a refresh
	// answer, and a provider that stops rotating simply leaves it out,
	// meaning keep the one you have. Writing the absence through would
	// store an empty refresh over a token already spent at the provider,
	// which cannot be undone and is not noticed until the next renewal
	// hours later.
	refresh := decoded.RefreshToken
	if refresh == "" {
		refresh = refreshToken
	}

	return Tokens{
		Access:    decoded.AccessToken,
		Refresh:   refresh,
		ExpiresIn: clampExpiry(decoded.ExpiresIn),
	}, nil
}

// ///////////////////////////////////////////////
// Requests
// ///////////////////////////////////////////////

// postForm sends one form and returns the body.
//
// The values carry a device code and, on refresh, a refresh token. Neither
// reaches an error from here. A failure names the status or the transport
// error, and never the form, because an error is where a credential travels
// furthest.
func postForm(ctx context.Context, client *http.Client, endpoint string, values url.Values) ([]byte, error) {
	if client == nil {
		client = defaultClient()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// G704: the endpoint is a package constant. The credential
	// travels in the form body, which is never placed in a URL.
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("asking twitch: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("reading twitch's answer: %w", err)
	}

	// A 400 carries the pending and slow_down states, so it is not a
	// failure here. Anything at or above 500 is.
	if response.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("twitch answered %d", response.StatusCode)
	}
	// Reported so a caller can tell a settled refusal from a bad moment. A
	// 429 body carries no access token either, and reading its absence as
	// "the grant is dead" tells the operator to authorize again over a rate
	// limit that clears on its own.
	if response.StatusCode == http.StatusTooManyRequests {
		return body, fmt.Errorf("%w: twitch answered %d",
			ErrTransient, response.StatusCode)
	}
	return body, nil
}
