package twitch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// redirectTransport sends every request to the test server.
type redirectTransport struct {
	base  string
	inner http.RoundTripper
}

// sentinelToken stands in for a credential. Obviously fake, fixed length,
// and recognisable in any output a test captures.
const sentinelToken = "EXAMPLETOKENEXAMPLETOKEN123456"

// validBody is what Twitch answers for a live browser token. A browser
// session token reports expires_in 0, meaning no announced expiry.
const validBody = `{"client_id":"exampleclientid","login":"examplechannel",` +
	`"user_id":"100001","scopes":[],"expires_in":0}`

// serving returns a client pointed at a handler, and closes the server on
// cleanup.
func serving(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Every request is redirected to the test server, so no case here can
	// reach Twitch even if a URL constant is wrong.
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &redirectTransport{base: server.URL, inner: server.Client().Transport},
	}
}

// RoundTrip implements http.RoundTripper.
//
// The query survives the rewrite. Helix takes its arguments there. A
// transport that dropped it would hand every case the same request, and the
// assertions about what was asked for could not fail.
func (r *redirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	rewritten := request.Clone(request.Context())
	target := r.base + request.URL.Path
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	//nolint:gosec // G704: r.base is the httptest server this test started.
	parsed, err := http.NewRequest(request.Method, target, nil)
	if err != nil {
		return nil, err
	}
	rewritten.URL = parsed.URL
	rewritten.Host = parsed.Host
	return r.inner.RoundTrip(rewritten)
}

// ///////////////////////////////////////////////
// Validate
// ///////////////////////////////////////////////

func TestValidate_ReadsTheIdentityBack(t *testing.T) {
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validBody))
	})

	got, err := Validate(context.Background(), client, sentinelToken)
	if err != nil {
		t.Fatalf("Validate() err = %v, want nil", err)
	}
	if got.Login != "examplechannel" {
		t.Errorf("Login = %q, want %q", got.Login, "examplechannel")
	}
	if got.ClientID != "exampleclientid" {
		t.Errorf("ClientID = %q, want %q", got.ClientID, "exampleclientid")
	}
	if got.ExpiresIn != 0 {
		t.Errorf("ExpiresIn = %v, want 0 for a browser session token", got.ExpiresIn)
	}
}

func TestValidate_SendsTheTokenInAHeaderAndNotTheURL(t *testing.T) {
	// A token in a query string reaches proxy logs, server logs, and this
	// process's own error strings, none of which are places a credential
	// can be recalled from.
	var seenURL, seenAuth string
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.String()
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validBody))
	})

	if _, err := Validate(context.Background(), client, sentinelToken); err != nil {
		t.Fatalf("Validate() err = %v, want nil", err)
	}

	if strings.Contains(seenURL, sentinelToken) {
		t.Errorf("the token reached the URL: %q", seenURL)
	}
	if seenAuth != "OAuth "+sentinelToken {
		t.Errorf("Authorization = %q, want the OAuth form", seenAuth)
	}
}

func TestValidate_ReportsARefusedTokenDistinctly(t *testing.T) {
	// A refused token means "ask the operator for a new one". Anything else
	// means the question could not be put, and must not delete a credential
	// that may still be good.
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":401,"message":"invalid access token"}`))
	})

	_, err := Validate(context.Background(), client, sentinelToken)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Validate() err = %v, want ErrInvalidToken", err)
	}
}

func TestValidate_TreatsAServerFailureAsUnknownRatherThanRefused(t *testing.T) {
	// The distinction that stops a Twitch outage from deleting a working
	// credential on every machine running this.
	tests := []struct {
		name   string
		status int
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "bad gateway", status: http.StatusBadGateway},
		{name: "rate limited", status: http.StatusTooManyRequests},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})

			_, err := Validate(context.Background(), client, sentinelToken)
			if err == nil {
				t.Fatal("Validate() err = nil, want a failure")
			}
			if errors.Is(err, ErrInvalidToken) {
				t.Errorf("Validate() read %d as a refused token", tt.status)
			}
		})
	}
}

func TestValidate_ReportsAnUnreadableBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"login":`},
		{name: "empty", body: ``},
		{name: "not an object", body: `["examplechannel"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			})

			if _, err := Validate(context.Background(), client, sentinelToken); err == nil {
				t.Errorf("Validate() err = nil for a %s body, want a failure", tt.name)
			}
		})
	}
}

func TestValidate_HonoursCancellation(t *testing.T) {
	// An interactive command must answer Ctrl-C at once rather than at the
	// end of a network timeout.
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Validate(ctx, client, sentinelToken); err == nil {
		t.Error("Validate() err = nil for a cancelled context, want a failure")
	}
}

// ///////////////////////////////////////////////
// The leak the whole package exists to prevent
// ///////////////////////////////////////////////

func TestValidate_NoFailurePathCarriesTheToken(t *testing.T) {
	// An error gets wrapped, logged, and pasted into a bug report, so a
	// token inside one travels further than a token anywhere else.
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "refused",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "malformed body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{`))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(context.Background(), serving(t, tt.handler), sentinelToken)
			if err == nil {
				t.Fatal("Validate() err = nil, want a failure to inspect")
			}
			if strings.Contains(err.Error(), sentinelToken) {
				t.Errorf("the error carries the token: %q", err.Error())
			}
		})
	}
}

func TestValidate_AnUnreachableServerDoesNotCarryTheToken(t *testing.T) {
	// The transport's own error is the one most likely to quote the whole
	// request, so it gets its own case rather than sharing the table above.
	// The server is started and immediately closed, so the dial fails
	// locally rather than reaching for the real id.twitch.tv.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := &http.Client{
		Timeout:   time.Second,
		Transport: &redirectTransport{base: dead.URL, inner: dead.Client().Transport},
	}
	dead.Close()

	_, err := Validate(context.Background(), client, sentinelToken)
	if err == nil {
		t.Skip("the request unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("the transport error carries the token: %q", err.Error())
	}
}
