package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/secret"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// toTestServer sends every request to the local server.
type toTestServer struct {
	base  string
	inner http.RoundTripper
}

// authToken is the credential every case below hands the command. It is
// obviously fake and fixed length, so it is recognisable anywhere it must not
// appear.
const authToken = "EXAMPLETOKENEXAMPLETOKEN123456"

// validIdentity is what Twitch answers for a live browser token. The client
// id is its own website's, which is the only one its playback endpoint
// accepts.
const validIdentity = `{"client_id":"` + twitch.WebClientID + `","login":"examplechannel",` +
	`"user_id":"100001","scopes":[],"expires_in":0}`

// foreignIdentity is a token that validates cleanly and belongs to some
// other application, which is exactly what a device-flow token looks like
// here and exactly what will not play a stream.
const foreignIdentity = `{"client_id":"someotherapplication","login":"examplechannel",` +
	`"user_id":"100001","scopes":[],"expires_in":0}`

// authTest builds deps pointed at a local server and a temporary data dir.
func authTest(t *testing.T, status int, body string) (authDeps, string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	return authDeps{
		store: secret.NewMemory(),
		// Every request is rewritten to the test server. Without this the
		// validation reaches the real id.twitch.tv, which no test may do:
		// it is a live request to a third party from `go test`.
		client: &http.Client{Transport: &toTestServer{
			base:  server.URL,
			inner: server.Client().Transport,
		}},
		dataDir: dir,
		prompt:  func(io.Writer) (string, error) { return authToken, nil },
	}, dir
}

// RoundTrip implements http.RoundTripper.
func (r *toTestServer) RoundTrip(request *http.Request) (*http.Response, error) {
	//nolint:gosec // G704: r.base is the httptest server this test started.
	rewritten, err := http.NewRequestWithContext(request.Context(),
		request.Method, r.base+request.URL.Path, nil)
	if err != nil {
		return nil, err
	}
	rewritten.Header = request.Header
	return r.inner.RoundTrip(rewritten)
}

// ///////////////////////////////////////////////
// The leak this command exists to avoid
// ///////////////////////////////////////////////

func TestRunAuthLogin_NeverPrintsTheToken(t *testing.T) {
	// The operator is watching this command run, and its output is the kind
	// of thing pasted into a bug report or captured on a screen share.
	deps, _ := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogin() err = %v, want nil", err)
	}

	if strings.Contains(out.String(), authToken) {
		t.Errorf("the command printed the token:\n%s", out.String())
	}
}

func TestRunAuthLogin_RefusesATokenFromAnotherApplication(t *testing.T) {
	// Validating and playing are two different judges. Twitch's validation
	// endpoint accepts any user token and names the application it belongs
	// to; its playback endpoint accepts only its own website's. A token from
	// anywhere else stores cleanly and then records nothing at all, while
	// the hourly credential check confirms it is healthy every hour.
	deps, dir := authTest(t, http.StatusOK, foreignIdentity)

	var out bytes.Buffer
	err := runAuthLogin(context.Background(), &out, deps)

	if err == nil {
		t.Fatal("runAuthLogin() err = nil, want a token from another application refused")
	}
	if !strings.Contains(err.Error(), "cookie") {
		t.Errorf("err = %q, want it to say where a working token comes from", err.Error())
	}
	if _, err := os.Stat(filepath.Join(dir, twitch.AuthConfigName)); err == nil {
		t.Error("a token the platform will not play was written to the recorder's config")
	}
	if _, err := deps.store.Get(secret.AccountTwitch); !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("stored err = %v, want the refused token kept out of the store", err)
	}
}

func TestRunAuthLogin_AcceptsTheWebClientsOwnToken(t *testing.T) {
	// The companion, so the refusal above cannot be satisfied by refusing
	// everything.
	deps, dir := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogin() err = %v, want the browser cookie accepted", err)
	}
	if _, err := os.Stat(filepath.Join(dir, twitch.AuthConfigName)); err != nil {
		t.Errorf("the recorder's config was not written: %v", err)
	}
}

func TestRunAuthLogin_NoFailurePathPrintsOrReturnsTheToken(t *testing.T) {
	// Every way this can fail, checked against both the output and the
	// error. An error gets wrapped and logged, so it travels further than
	// anything written to the terminal.
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "refused", status: http.StatusUnauthorized, body: `{"status":401}`},
		{name: "server error", status: http.StatusInternalServerError, body: ``},
		{name: "malformed", status: http.StatusOK, body: `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _ := authTest(t, tt.status, tt.body)

			var out bytes.Buffer
			err := runAuthLogin(context.Background(), &out, deps)
			if err == nil {
				t.Fatal("runAuthLogin() err = nil, want a failure")
			}
			if strings.Contains(err.Error(), authToken) {
				t.Errorf("the error carries the token: %q", err.Error())
			}
			if strings.Contains(out.String(), authToken) {
				t.Errorf("the output carries the token:\n%s", out.String())
			}
		})
	}
}

// ///////////////////////////////////////////////
// What a login leaves behind
// ///////////////////////////////////////////////

func TestRunAuthLogin_StoresTheTokenAndDerivesTheConfig(t *testing.T) {
	// Two artefacts, and both are needed. The store is the durable source of
	// truth an interactive session owns. The derived file is what the daemon
	// can read under an S4U logon.
	deps, dir := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogin() err = %v, want nil", err)
	}

	stored, err := deps.store.Get(secret.AccountTwitch)
	if err != nil {
		t.Fatalf("the token was not stored: %v", err)
	}
	if stored != authToken {
		t.Errorf("stored %q, want the entered token", stored)
	}
	if _, err := os.Stat(filepath.Join(dir, twitch.AuthConfigName)); err != nil {
		t.Errorf("the derived streamlink config was not written: %v", err)
	}
}

func TestRunAuthLogin_StoresNothingWhenTwitchRefuses(t *testing.T) {
	// Storing first would leave a dead credential behind and a derived file
	// handing it to every capture.
	deps, dir := authTest(t, http.StatusUnauthorized, `{"status":401}`)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err == nil {
		t.Fatal("runAuthLogin() err = nil for a refused token, want a failure")
	}

	if _, err := deps.store.Get(secret.AccountTwitch); !errors.Is(err, secret.ErrNotFound) {
		t.Error("a refused token was stored")
	}
	if _, err := os.Stat(filepath.Join(dir, twitch.AuthConfigName)); err == nil {
		t.Error("a refused token produced a streamlink config")
	}
}

func TestRunAuthLogin_SaysWhatTheTokenGrants(t *testing.T) {
	// streamlink's own docs say these tokens grant full account access.
	// Someone pasting one is owed that, and how to take it back.
	deps, _ := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogin() err = %v, want nil", err)
	}

	if !strings.Contains(out.String(), "Sign Out Everywhere") {
		t.Errorf("the output does not say how to revoke the token:\n%s", out.String())
	}
}

func TestRunAuthLogin_RefusesAnEmptyEntry(t *testing.T) {
	// Pressing enter at the prompt must not write an auth config saying
	// OAuth and nothing else, which streamlink would send on every capture.
	deps, dir := authTest(t, http.StatusOK, validIdentity)
	deps.prompt = func(io.Writer) (string, error) { return "   ", nil }

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err == nil {
		t.Fatal("runAuthLogin() err = nil for an empty entry, want a refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, twitch.AuthConfigName)); err == nil {
		t.Error("an empty entry produced a streamlink config")
	}
}

// ///////////////////////////////////////////////
// status
// ///////////////////////////////////////////////

func TestRunAuthStatus_ReportsNoTokenWithoutFailing(t *testing.T) {
	// The state of every machine before the operator authenticates. An
	// error here would make a fresh install look broken.
	deps, _ := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthStatus() err = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "none stored") {
		t.Errorf("output did not report the absent token:\n%s", out.String())
	}
}

func TestRunAuthStatus_ReportsADeadTokenWithoutFailing(t *testing.T) {
	// status answers a question. A dead token is the answer, not an error,
	// so this stays usable in a script that checks before recording.
	deps, _ := authTest(t, http.StatusUnauthorized, `{"status":401}`)
	if err := deps.store.Set(secret.AccountTwitch, authToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	var out bytes.Buffer
	if err := runAuthStatus(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthStatus() err = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "no longer works") {
		t.Errorf("output did not report the dead token:\n%s", out.String())
	}
	if strings.Contains(out.String(), authToken) {
		t.Errorf("status printed the token:\n%s", out.String())
	}
}

// ///////////////////////////////////////////////
// logout
// ///////////////////////////////////////////////

func TestRunAuthLogout_RemovesBothTheStoreEntryAndTheDerivedFile(t *testing.T) {
	// Leaving the derived file would keep handing a credential the operator
	// asked to forget to every capture.
	deps, dir := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogin() err = %v, want nil", err)
	}
	if err := runAuthLogout(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogout() err = %v, want nil", err)
	}

	if _, err := deps.store.Get(secret.AccountTwitch); !errors.Is(err, secret.ErrNotFound) {
		t.Error("the token survived logout")
	}
	if _, err := os.Stat(filepath.Join(dir, twitch.AuthConfigName)); err == nil {
		t.Error("the derived streamlink config survived logout")
	}
}

func TestRunAuthLogout_IsSafeToRepeat(t *testing.T) {
	// Anyone unsure whether the first took runs it again.
	deps, _ := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	for range 2 {
		if err := runAuthLogout(context.Background(), &out, deps); err != nil {
			t.Errorf("runAuthLogout() err = %v, want nil", err)
		}
	}
}

// ///////////////////////////////////////////////
// The doctor row
// ///////////////////////////////////////////////

func TestCredentialRow_ReportsAnAbsentTokenWithoutFailing(t *testing.T) {
	// Recording public streams is a supported way to run this, and a first
	// install has no credential by definition. A failure row here would
	// make doctor report a problem on a machine that has none.
	got := credentialRow(t.TempDir(), false)

	if !strings.Contains(joined(got), "no token stored") {
		t.Errorf("credentialRow() = %v, want it to report the absent token", joined(got))
	}
	if got.State == outcomeFail {
		t.Errorf("credentialRow() = %v, want no failure marker for an absent token", joined(got))
	}
}

func TestCredentialRow_ReportsAStoredToken(t *testing.T) {
	dir := t.TempDir()
	if _, err := twitch.WriteAuthConfig(dir, authToken); err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}

	got := credentialRow(dir, false)

	if !strings.Contains(joined(got), "a token is stored") {
		t.Errorf("credentialRow() = %v, want it to report the stored token", joined(got))
	}
}

func TestCredentialRow_NeverPrintsTheTokenAtAnyVerbosity(t *testing.T) {
	// doctor output is the single most likely thing to be pasted into a bug
	// report, and the row is read straight off a file that holds a token.
	dir := t.TempDir()
	if _, err := twitch.WriteAuthConfig(dir, authToken); err != nil {
		t.Fatalf("WriteAuthConfig() err = %v, want nil", err)
	}

	for _, verbose := range []bool{false, true} {
		row := joined(credentialRow(dir, verbose))
		if strings.Contains(row, authToken) {
			t.Errorf("credentialRow(verbose=%v) printed the token: %q", verbose, row)
		}
	}
}

// ///////////////////////////////////////////////
// The snippet the operator pastes
// ///////////////////////////////////////////////

func TestRunAuthLogin_TheSnippetIsSelectableAsItself(t *testing.T) {
	// The operator selects this line with a mouse and pastes it into a
	// browser console. Anything before it on the line is dragged in with it,
	// and a trailing period pastes as part of the expression.
	deps, _ := authTest(t, http.StatusOK, validIdentity)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runAuthLogin() err = %v, want nil", err)
	}

	found := false
	for line := range strings.SplitSeq(out.String(), "\n") {
		if !strings.Contains(line, "document.cookie") {
			continue
		}
		found = true
		if line != browserSnippet {
			t.Errorf("the snippet line = %q, want the snippet and nothing else", line)
		}
	}
	if !found {
		t.Errorf("the snippet never printed:\n%s", out.String())
	}
}

func TestRunMetadataLogin_StatesTheCodeOnlyWhereThePageWillNotCarryIt(t *testing.T) {
	// Twitch puts the code in the page's own URL, so telling the operator to
	// enter it is telling them to type over something already filled in. A
	// provider that stops filling the page has to degrade on its own.
	tests := []struct {
		name     string
		page     string
		carries  bool
		wantCode bool
	}{
		{
			name:     "the page carries the code",
			page:     "https://www.twitch.tv/activate?device-code=ABCDEFGH",
			carries:  true,
			wantCode: false,
		},
		{
			name:     "a bare page, so the code has to be typed",
			page:     "https://www.twitch.tv/activate",
			carries:  false,
			wantCode: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := renderMetadataPrompt(t, tt.page, "ABCDEFGH", tt.carries)

			if !strings.Contains(out, tt.page) {
				t.Errorf("output never named the page:\n%s", out)
			}
			if got := strings.Contains(out, "enter the code"); got != tt.wantCode {
				t.Errorf("output tells the operator to enter the code = %t, want %t:\n%s",
					got, tt.wantCode, out)
			}
		})
	}
}

// renderMetadataPrompt drives the real command against a scripted Twitch and
// returns what it printed.
//
// The device flow is substituted rather than the whole command, because
// StartDevice refuses without a build-time application id and the branch
// worth testing is the one after it. The poll and the validation are answered
// by a local server, so what runs here is the command itself.
func renderMetadataPrompt(t *testing.T, page, code string, carries bool) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"EXAMPLEACCESSEXAMPLEACCESS1234",`+
				`"refresh_token":"EXAMPLEREFRESHEXAMPLEREFRESH12","expires_in":14400}`)
			return
		}
		fmt.Fprint(w, validIdentity)
	}))
	t.Cleanup(server.Close)

	deps := authDeps{
		store: secret.NewMemory(),
		client: &http.Client{Transport: &toTestServer{
			base:  server.URL,
			inner: server.Client().Transport,
		}},
		dataDir:  t.TempDir(),
		clientID: "exampleclientid",
		startDevice: func(context.Context, *http.Client, string) (twitch.DeviceCode, error) {
			return twitch.DeviceCode{
				DeviceCode:      "DEVICECODEDEVICECODE1234",
				UserCode:        code,
				VerificationURI: page,
				CodeIsInTheURI:  carries,
				ExpiresIn:       30 * time.Minute,
				Interval:        time.Millisecond,
			}, nil
		},
	}

	var out bytes.Buffer
	if err := runMetadataLogin(context.Background(), &out, deps); err != nil {
		t.Fatalf("runMetadataLogin() err = %v, want nil", err)
	}
	return out.String()
}
