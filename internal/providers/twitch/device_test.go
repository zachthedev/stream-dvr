package twitch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// scriptedTwitch answers each poll from a list, so a case can spell out the
// sequence Twitch would send.
type scriptedTwitch struct {
	mu sync.Mutex
	// replies are returned in order. The last one repeats.
	replies []string
	calls   int
	// seen records each request's form, so a test can assert what was sent
	// without reaching into the client.
	seen []url.Values
	// gaps records the delay before each poll after the first, which is
	// what proves an interval was honoured.
	gaps []time.Duration
	last time.Time
}

// testClientID is the application id these tests act as. It is passed in
// rather than installed, because the client id is config now.
const testClientID = "exampleclientid"

// grantedBody is what Twitch answers once the operator authorizes the code.
const grantedBody = `{"access_token":"ACCESSTOKENACCESSTOKEN12345678",` +
	`"refresh_token":"REFRESHTOKENREFRESHTOKEN123456","expires_in":14124}`

// serve returns a client wired to the script.
func (s *scriptedTwitch) serve(t *testing.T) *http.Client {
	t.Helper()

	return serving(t, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		if !s.last.IsZero() {
			s.gaps = append(s.gaps, time.Since(s.last))
		}
		s.last = time.Now()

		_ = r.ParseForm()
		s.seen = append(s.seen, r.Form)

		reply := s.replies[min(s.calls, len(s.replies)-1)]
		s.calls++
		_, _ = w.Write([]byte(reply))
	})
}

// fastCode is a flow with a short interval, so a polling test finishes in
// milliseconds rather than at Twitch's real cadence.
func fastCode() DeviceCode {
	return DeviceCode{
		DeviceCode:      "DEVICECODEDEVICECODE1234",
		UserCode:        "ABCD-EFGH",
		VerificationURI: "https://www.twitch.tv/activate",
		ExpiresIn:       2 * time.Second,
		Interval:        10 * time.Millisecond,
	}
}

// ///////////////////////////////////////////////
// StartDevice
// ///////////////////////////////////////////////

func TestStartDevice_ReportsNoConfiguredClientID(t *testing.T) {
	// A client id is injected at build time, so a developer build has none.
	// Saying so beats a request Twitch refuses for reasons that read like a
	// network problem.

	if _, err := StartDevice(context.Background(), nil, ""); !errors.Is(err, ErrNoClientID) {
		t.Errorf("StartDevice() err = %v, want ErrNoClientID", err)
	}
}

func TestStartDevice_AsksForNoScopes(t *testing.T) {
	// Helix Get Videos and Get Users accept any valid user token without a
	// scope. Requesting one the code never exercises asks the operator for
	// account access with no reason to show them.
	script := &scriptedTwitch{replies: []string{
		`{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCD-EFGH",` +
			`"verification_uri":"https://www.twitch.tv/activate","expires_in":1800,"interval":5}`,
	}}

	if _, err := StartDevice(context.Background(), script.serve(t), testClientID); err != nil {
		t.Fatalf("StartDevice() err = %v, want nil", err)
	}

	if got := script.seen[0].Get("scopes"); got != "" {
		t.Errorf("scopes = %q, want empty", got)
	}
}

func TestStartDevice_CarriesTheServersOwnVerificationPage(t *testing.T) {
	// Twitch is free to move the page. An operator sent to a constant
	// written here is stuck at a URL that does not take their code.
	script := &scriptedTwitch{replies: []string{
		`{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCD-EFGH",` +
			`"verification_uri":"https://example.com/somewhere-else","expires_in":1800,"interval":5}`,
	}}

	code, err := StartDevice(context.Background(), script.serve(t), testClientID)
	if err != nil {
		t.Fatalf("StartDevice() err = %v, want nil", err)
	}
	if code.VerificationURI != "https://example.com/somewhere-else" {
		t.Errorf("VerificationURI = %q, want the server's own", code.VerificationURI)
	}
}

func TestStartDevice_RefusesAResponseWithNoCode(t *testing.T) {
	// Polling with an empty device code would ask forever about nothing.
	script := &scriptedTwitch{replies: []string{`{"expires_in":1800,"interval":5}`}}

	if _, err := StartDevice(context.Background(), script.serve(t), testClientID); err == nil {
		t.Error("StartDevice() err = nil for a response with no code, want a failure")
	}
}

// ///////////////////////////////////////////////
// PollDevice
// ///////////////////////////////////////////////

func TestPollDevice_WaitsThroughPendingAndReturnsTheGrant(t *testing.T) {
	// The ordinary flow: the operator takes a moment to enter the code.
	script := &scriptedTwitch{replies: []string{
		`{"message":"authorization_pending"}`,
		`{"message":"authorization_pending"}`,
		grantedBody,
	}}

	tokens, err := PollDevice(context.Background(), script.serve(t), fastCode(), testClientID)
	if err != nil {
		t.Fatalf("PollDevice() err = %v, want nil", err)
	}
	if tokens.Access == "" || tokens.Refresh == "" {
		t.Errorf("PollDevice() returned %+v, want both tokens", tokens)
	}
	if script.calls < 3 {
		t.Errorf("polled %d times, want it to wait through both pending replies", script.calls)
	}
}

func TestPollDevice_WidensTheIntervalWhenAskedToSlowDown(t *testing.T) {
	// RFC 8628: slow_down adds to the interval. Holding the old cadence
	// earns a refusal from a server that just said it is being asked too
	// often.
	script := &scriptedTwitch{replies: []string{
		`{"message":"slow_down"}`,
		grantedBody,
	}}

	code := fastCode()
	code.ExpiresIn = 30 * time.Second
	if _, err := PollDevice(context.Background(), script.serve(t), code, testClientID); err != nil {
		t.Fatalf("PollDevice() err = %v, want nil", err)
	}

	if len(script.gaps) == 0 {
		t.Fatal("no interval was measured")
	}
	// The poll after slow_down must wait longer than the original interval.
	if script.gaps[0] < slowDownStep {
		t.Errorf("waited %v after slow_down, want at least %v", script.gaps[0], slowDownStep)
	}
}

func TestPollDevice_ReportsACodeThatExpired(t *testing.T) {
	// The operator walked away. Starting again is the whole remedy, and
	// saying so beats polling a dead code until the process is killed.
	script := &scriptedTwitch{replies: []string{`{"message":"authorization_pending"}`}}

	code := fastCode()
	code.ExpiresIn = 20 * time.Millisecond

	_, err := PollDevice(context.Background(), script.serve(t), code, testClientID)
	if !errors.Is(err, ErrDeviceCodeExpired) {
		t.Errorf("PollDevice() err = %v, want ErrDeviceCodeExpired", err)
	}
}

func TestPollDevice_ReportsARefusalAtThePage(t *testing.T) {
	// The operator said no. Retrying would be asking them again on a timer.
	script := &scriptedTwitch{replies: []string{`{"message":"access_denied"}`}}

	_, err := PollDevice(context.Background(), script.serve(t), fastCode(), testClientID)
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Errorf("PollDevice() err = %v, want ErrAuthorizationDenied", err)
	}
}

func TestPollDevice_StopsAtOnceOnCancellation(t *testing.T) {
	// Ctrl-C during a wait must end the command now, not at the next tick.
	script := &scriptedTwitch{replies: []string{`{"message":"authorization_pending"}`}}

	code := fastCode()
	code.Interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { _, err := PollDevice(ctx, script.serve(t), code, testClientID); done <- err }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("PollDevice() err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PollDevice did not answer a cancelled context")
	}
}

func TestPollDevice_NoFailureCarriesTheDeviceCode(t *testing.T) {
	// The device code is a credential for the life of the flow, and every
	// failure here reaches a terminal.
	script := &scriptedTwitch{replies: []string{`{"message":"access_denied"}`}}

	code := fastCode()
	_, err := PollDevice(context.Background(), script.serve(t), code, testClientID)
	if err == nil {
		t.Fatal("PollDevice() err = nil, want a failure")
	}
	if strings.Contains(err.Error(), code.DeviceCode) {
		t.Errorf("the error carries the device code: %q", err.Error())
	}
}

// ///////////////////////////////////////////////
// Refresh
// ///////////////////////////////////////////////

func TestRefresh_ReturnsBothHalvesOfTheNewPair(t *testing.T) {
	// A refresh token is ONE TIME USE, so the caller must store what comes
	// back before discarding what it passed in. Returning only the access
	// token would make that impossible and lose the session on the next
	// refresh.
	script := &scriptedTwitch{replies: []string{grantedBody}}

	tokens, err := Refresh(context.Background(), script.serve(t), "OLDREFRESHOLDREFRESH123456", testClientID)
	if err != nil {
		t.Fatalf("Refresh() err = %v, want nil", err)
	}
	if tokens.Access == "" {
		t.Error("Refresh() returned no access token")
	}
	if tokens.Refresh == "" {
		t.Error("Refresh() returned no refresh token, so the next refresh would have nothing to use")
	}
}

func TestRefresh_ReportsARefusedRefreshToken(t *testing.T) {
	// Twitch expires a refresh token after 30 days idle. A daemon off that
	// long must re-authenticate, and this is how it learns.
	script := &scriptedTwitch{replies: []string{`{"message":"Invalid refresh token"}`}}

	_, err := Refresh(context.Background(), script.serve(t), "DEADREFRESHDEADREFRESH1234", testClientID)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Refresh() err = %v, want ErrInvalidToken", err)
	}
}

func TestRefresh_NoFailureCarriesTheRefreshToken(t *testing.T) {
	script := &scriptedTwitch{replies: []string{`{"message":"Invalid refresh token"}`}}

	const refresh = "DEADREFRESHDEADREFRESH1234"
	_, err := Refresh(context.Background(), script.serve(t), refresh, testClientID)
	if err == nil {
		t.Fatal("Refresh() err = nil, want a failure")
	}
	if strings.Contains(err.Error(), refresh) {
		t.Errorf("the error carries the refresh token: %q", err.Error())
	}
}

func TestRefresh_ReportsNoConfiguredClientID(t *testing.T) {
	if _, err := Refresh(context.Background(), nil, "anything", ""); !errors.Is(err, ErrNoClientID) {
		t.Errorf("Refresh() err = %v, want ErrNoClientID", err)
	}
}

// ///////////////////////////////////////////////
// Which page the operator opens
// ///////////////////////////////////////////////

func TestStartDevice_PrefersThePageThatAlreadyCarriesTheCode(t *testing.T) {
	// RFC 8628 makes verification_uri_complete optional. A provider that
	// sends one is offering a page the operator can open and be done with,
	// so it wins over the bare one.
	script := &scriptedTwitch{replies: []string{
		`{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCDEFGH",` +
			`"verification_uri":"https://example.com/activate",` +
			`"verification_uri_complete":"https://example.com/activate?code=ABCDEFGH",` +
			`"expires_in":1800,"interval":5}`,
	}}

	code, err := StartDevice(context.Background(), script.serve(t), testClientID)
	if err != nil {
		t.Fatalf("StartDevice() err = %v, want nil", err)
	}

	if code.VerificationURI != "https://example.com/activate?code=ABCDEFGH" {
		t.Errorf("VerificationURI = %q, want the page carrying the code", code.VerificationURI)
	}
	if !code.CodeIsInTheURI {
		t.Error("CodeIsInTheURI = false, want true for a page that carries the code")
	}
}

func TestStartDevice_ReportsWhetherThePageCarriesTheCode(t *testing.T) {
	// The answer is settled where the page is chosen, not worked out later
	// from the two strings. A caller that believes a bare page carries the
	// code withholds the one thing the operator needs, and the flow stalls
	// with nothing on screen to act on.
	tests := []struct {
		name  string
		reply string
		want  bool
	}{
		{
			name: "the RFC field, which is the signal",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCDEFGH",` +
				`"verification_uri":"https://example.com/activate",` +
				`"verification_uri_complete":"https://example.com/activate?code=ABCDEFGH",` +
				`"expires_in":1800,"interval":5}`,
			want: true,
		},
		{
			name: "no RFC field, code filled into a query parameter",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"YFVCQGBP",` +
				`"verification_uri":"https://www.twitch.tv/activate?device-code=YFVCQGBP",` +
				`"expires_in":1800,"interval":5}`,
			want: true,
		},
		{
			name: "a bare page, so the code has to be stated",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCDEFGH",` +
				`"verification_uri":"https://www.twitch.tv/activate",` +
				`"expires_in":1800,"interval":5}`,
			want: false,
		},
		{
			name: "a page filled with somebody else's code",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCDEFGH",` +
				`"verification_uri":"https://www.twitch.tv/activate?device-code=ZZZZZZZZ",` +
				`"expires_in":1800,"interval":5}`,
			want: false,
		},
		{
			name: "a code short enough to occur in the address by chance",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"tv",` +
				`"verification_uri":"https://www.twitch.tv/activate",` +
				`"expires_in":1800,"interval":5}`,
			want: false,
		},
		{
			name: "the code spelled out in the host",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"activate",` +
				`"verification_uri":"https://www.twitch.tv/activate",` +
				`"expires_in":1800,"interval":5}`,
			want: false,
		},
		{
			name: "the code in a fragment, which a browser never sends",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCDEFGH",` +
				`"verification_uri":"https://www.twitch.tv/activate#ABCDEFGH",` +
				`"expires_in":1800,"interval":5}`,
			want: false,
		},
		{
			name: "the code buried in an unrelated parameter value",
			reply: `{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCDEFGH",` +
				`"verification_uri":"https://www.twitch.tv/activate?ref=go-ABCDEFGH-home",` +
				`"expires_in":1800,"interval":5}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := &scriptedTwitch{replies: []string{tt.reply}}

			code, err := StartDevice(context.Background(), script.serve(t), testClientID)
			if err != nil {
				t.Fatalf("StartDevice() err = %v, want nil", err)
			}
			if code.CodeIsInTheURI != tt.want {
				t.Errorf("CodeIsInTheURI = %t, want %t for %s",
					code.CodeIsInTheURI, tt.want, code.VerificationURI)
			}
		})
	}
}

// ///////////////////////////////////////////////
// The configured application reaches Twitch
// ///////////////////////////////////////////////

func TestStartDevice_SendsTheApplicationItWasGiven(t *testing.T) {
	// The id is config now, so the value an operator sets is the one that
	// has to arrive. A build that carried its own would look identical here
	// until two installs shared a registration.
	script := &scriptedTwitch{replies: []string{
		`{"device_code":"DEVICECODEDEVICECODE1234","user_code":"ABCD-EFGH",` +
			`"verification_uri":"https://www.twitch.tv/activate","expires_in":1800,"interval":5}`,
	}}

	if _, err := StartDevice(context.Background(), script.serve(t), "operatorsownid"); err != nil {
		t.Fatalf("StartDevice() err = %v, want nil", err)
	}

	if len(script.seen) != 1 {
		t.Fatalf("saw %d requests, want 1", len(script.seen))
	}
	if got := script.seen[0].Get("client_id"); got != "operatorsownid" {
		t.Errorf("client_id = %q, want the id the caller passed", got)
	}
}

func TestRefresh_SendsTheApplicationItWasGiven(t *testing.T) {
	// Refresh runs unattended for as long as the recorder does, so an id
	// that only reached the first exchange would strand every later one.
	script := &scriptedTwitch{replies: []string{grantedBody}}

	if _, err := Refresh(context.Background(), script.serve(t),
		"OLDREFRESHOLDREFRESH123456", "operatorsownid"); err != nil {
		t.Fatalf("Refresh() err = %v, want nil", err)
	}

	if len(script.seen) != 1 {
		t.Fatalf("saw %d requests, want 1", len(script.seen))
	}
	if got := script.seen[0].Get("client_id"); got != "operatorsownid" {
		t.Errorf("client_id = %q, want the id the caller passed", got)
	}
}
