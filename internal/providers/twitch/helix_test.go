package twitch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// fakeVault is a Vault held in the test.
type fakeVault struct {
	mu sync.Mutex
	// value is what Get answers. Empty with no getErr means nothing stored.
	value string
	// writes records every value Set was given, in order, so a case can
	// assert what was persisted and when.
	writes []string
	getErr error
	setErr error
}

// helixServer answers Helix requests from a script.
type helixServer struct {
	mu sync.Mutex
	// users is the Get Users reply.
	users string
	// videos is the Get Videos reply.
	videos string
	// streams is the Get Streams reply.
	streams string
	// pages answers each videos request in turn, with the last repeating.
	// It is what a paged listing walks.
	pages []string
	// refuseFirst makes the first videos request answer 401, which is what
	// a token Twitch refuses looks like.
	refuseFirst bool
	// seen records the path and query of every request.
	seen []*url.URL
	// auth records the Authorization header of every request.
	auth []string
	// videoCalls counts requests to the videos endpoint.
	videoCalls int
	// userCalls counts requests to the users endpoint.
	userCalls int
	// streamCalls counts requests to the streams endpoint.
	streamCalls int
}

// refusingTransport fails any request, and says which one.
type refusingTransport struct{ t *testing.T }

// fixedSource is a TokenSource with no store behind it.
type fixedSource struct {
	token string
	// renewed is what Renew answers, and how many times it was asked.
	renewed  string
	renewals int
	renewErr error
	tokenErr error
}

// firstPage and secondPage are one channel's archives across two pages,
// with the cursor that joins them.
const firstPage = `{"data":[
	{"id":"2100001","stream_id":"48211557693","title":"a broadcast",
	 "url":"https://www.twitch.tv/videos/2100001",
	 "created_at":"2026-03-01T20:00:00Z","duration":"3h8m33s"}
],"pagination":{"cursor":"cursor-1"}}`

const secondPage = `{"data":[
	{"id":"2100002","stream_id":"48211557694","title":"an older broadcast",
	 "url":"https://www.twitch.tv/videos/2100002",
	 "created_at":"2026-01-15T19:30:00Z","duration":"55m2s"}
],"pagination":{}}`

// endlessPage always offers another cursor, which is what a bug at either
// end looks like.
const endlessPage = `{"data":[
	{"id":"2100003","stream_id":"48211557695","title":"a broadcast",
	 "url":"https://www.twitch.tv/videos/2100003",
	 "created_at":"2026-03-01T20:00:00Z","duration":"1h"}
],"pagination":{"cursor":"never-ends"}}`

// examplePage is a Get Videos reply carrying two archives.
const examplePage = `{"data":[
	{"id":"2100001","stream_id":"48211557693","title":"a broadcast",
	 "url":"https://www.twitch.tv/videos/2100001",
	 "created_at":"2026-03-01T20:00:00Z","duration":"3h8m33s"},
	{"id":"2100002","stream_id":"48211557694","title":"another broadcast",
	 "url":"https://www.twitch.tv/videos/2100002",
	 "created_at":"2026-02-28T19:30:00Z","duration":"55m2s"}
]}`

// exampleUser is a Get Users reply for the fixture channel.
const exampleUser = `{"data":[{"id":"100001","login":"examplechannel"}]}`

// exampleArchive is the address ArchiveURL builds, which is what discovery
// passes in.
const exampleArchive = "https://twitch.tv/examplechannel/videos"

// exampleChannel is the live address, which is what a start is resolved
// from.
const exampleChannel = "https://twitch.tv/examplechannel"

// allArchives reaches back further than any fixture, so a listing walks the
// whole reply rather than stopping at a horizon.
var allArchives = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Get implements Vault.
func (f *fakeVault) Get(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return "", f.getErr
	}
	if f.value == "" {
		return "", errors.New("no secret is stored for that account")
	}
	return f.value, nil
}

// Set implements Vault.
func (f *fakeVault) Set(_, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.setErr != nil {
		return f.setErr
	}
	f.value = value
	f.writes = append(f.writes, value)
	return nil
}

// stored reports what the vault currently holds.
func (f *fakeVault) stored() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.value
}

// RoundTrip implements http.RoundTripper.
func (r refusingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.t.Errorf("asked twitch for %s where no request was expected", request.URL.Path)
	return nil, errors.New("no request was expected")
}

// refusingClient fails any request made through it.
//
// It is what makes "this asked Twitch nothing" an assertion rather than a
// hope: a case using it fails if the code reaches out at all.
func refusingClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{Transport: refusingTransport{t: t}}
}

// Token implements TokenSource.
func (f *fixedSource) Token(context.Context) (string, error) {
	if f.tokenErr != nil {
		return "", f.tokenErr
	}
	return f.token, nil
}

// Renew implements TokenSource.
func (f *fixedSource) Renew(context.Context) (string, error) {
	f.renewals++
	if f.renewErr != nil {
		return "", f.renewErr
	}
	return f.renewed, nil
}

// serve returns a client wired to this server.
func (h *helixServer) serve(t *testing.T) *http.Client {
	t.Helper()

	return serving(t, func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()

		h.seen = append(h.seen, r.URL)
		h.auth = append(h.auth, r.Header.Get("Authorization"))

		switch {
		case strings.HasSuffix(r.URL.Path, "/streams"):
			h.streamCalls++
			if h.refuseFirst && h.streamCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(h.streams))
		case strings.HasSuffix(r.URL.Path, "/users"):
			h.userCalls++
			_, _ = w.Write([]byte(h.users))
		case strings.HasSuffix(r.URL.Path, "/videos"):
			h.videoCalls++
			if h.refuseFirst && h.videoCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if len(h.pages) > 0 {
				_, _ = w.Write([]byte(h.pages[min(h.videoCalls-1, len(h.pages)-1)]))
				return
			}
			_, _ = w.Write([]byte(h.videos))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// query returns the query of the last request to a path.
func (h *helixServer) query(suffix string) url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, v := range slices.Backward(h.seen) {
		if strings.HasSuffix(v.Path, suffix) {
			return v.Query()
		}
	}
	return nil
}

// storedSessionIn decodes what a vault holds, for asserting on the halves.
func storedSessionIn(t *testing.T, vault *fakeVault) storedSession {
	t.Helper()

	session := NewSession(testClientID, vault, "test", nil)
	stored, err := session.load()
	if err != nil {
		t.Fatalf("the vault holds nothing readable: %v", err)
	}
	return stored
}

// authorized returns a vault holding a session with the given life left.
func authorized(t *testing.T, life time.Duration) *fakeVault {
	t.Helper()

	vault := &fakeVault{}
	session := NewSession(testClientID, vault, "test", nil)
	if err := session.Authorize(Tokens{
		Access:    sentinelToken,
		Refresh:   "REFRESHTOKENREFRESHTOKEN123456",
		ExpiresIn: life,
	}); err != nil {
		t.Fatalf("Authorize() err = %v, want nil", err)
	}
	return vault
}

// ///////////////////////////////////////////////
// Session
// ///////////////////////////////////////////////

func TestStored_AsksTwitchNothingAndRefreshesNothing(t *testing.T) {
	// The property that makes it safe for a status command. Token would
	// spend the one-time refresh token on a session this near expiry, so a
	// read-only caller using it would rotate the session just by reporting
	// on it. A client that fails any request is what proves the difference.
	vault := authorized(t, refreshWindow/2)
	session := NewSession(testClientID, vault, "test", refusingClient(t))

	expiresAt, stored := session.Stored()
	if !stored {
		t.Fatal("Stored() reported nothing kept, want the authorized session")
	}
	if expiresAt.IsZero() {
		t.Error("Stored() returned no expiry, so a caller cannot tell a live token from a dead one")
	}
	if len(vault.writes) != 1 {
		t.Errorf("the vault was written %d times, want only the original Authorize", len(vault.writes))
	}
}

func TestStored_ReportsNothingKept(t *testing.T) {
	cases := []struct {
		name  string
		vault *fakeVault
	}{
		{name: "nothing authorized", vault: &fakeVault{}},
		{name: "a file that cannot be read", vault: &fakeVault{value: "{not json"}},
		{name: "a session with no access token", vault: &fakeVault{value: `{"refresh":"x"}`}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// Every way of failing to read one leaves the caller with
			// nothing to use, so they answer alike rather than making a
			// status command distinguish states it cannot act on.
			if _, stored := NewSession(testClientID, testCase.vault, "test", nil).Stored(); stored {
				t.Error("Stored() reported a usable session, want none")
			}
		})
	}
}

func TestSession_ReportsAMachineWithNothingAuthorized(t *testing.T) {
	// The ordinary state. Metadata is an optimisation, so this must read as
	// "there is none" rather than as a failure that stops a recovery pass.
	session := NewSession(testClientID, &fakeVault{}, "test", nil)

	if _, err := session.Token(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Errorf("Token() err = %v, want ErrNoSession", err)
	}
}

func TestSession_ReportsAStoredSessionItCannotRead(t *testing.T) {
	// A truncated or hand-edited file must not read as a valid empty
	// session, which would send an empty bearer token to Twitch.
	session := NewSession(testClientID, &fakeVault{value: "{not json"}, "test", nil)

	if _, err := session.Token(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Errorf("Token() err = %v, want ErrNoSession", err)
	}
}

func TestSession_HandsBackATokenWithLifeLeftWithoutAskingTwitch(t *testing.T) {
	// A token good for hours is used as it is. Refreshing it would spend the
	// one-time refresh token on every pass for nothing.
	vault := authorized(t, 4*time.Hour)
	session := NewSession(testClientID, vault, "test", refusingClient(t))

	token, err := session.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() err = %v, want nil", err)
	}
	if token != sentinelToken {
		t.Errorf("Token() = %q, want the stored access token", token)
	}
}

func TestSession_RefreshesATokenInsideTheRefreshWindow(t *testing.T) {
	// A token that expires mid-pass costs a refusal and a retry on every
	// request after it, so it is renewed while it still has minutes on it.
	vault := authorized(t, refreshWindow/2)
	script := &scriptedTwitch{replies: []string{grantedBody}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	token, err := session.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() err = %v, want nil", err)
	}
	if token == sentinelToken {
		t.Error("Token() returned the old access token, want the refreshed one")
	}
}

func TestSession_StoresTheNewPairBeforeReturningTheAccessToken(t *testing.T) {
	// The rule the whole design rests on. A Twitch refresh token is spent
	// the moment the exchange succeeds, so a caller handed the new access
	// token before the write would, on a crash, leave a session nothing can
	// recover.
	vault := authorized(t, refreshWindow/2)
	vault.setErr = errors.New("the disk is full")
	script := &scriptedTwitch{replies: []string{grantedBody}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	token, err := session.Token(context.Background())
	if err == nil {
		t.Fatal("Token() err = nil when the new pair could not be stored, want a failure")
	}
	if token != "" {
		t.Errorf("Token() = %q after a failed write, want nothing usable", token)
	}
}

func TestSession_KeepsBothHalvesOfTheRefreshedPair(t *testing.T) {
	// Storing only the access half would leave the next refresh with nothing
	// to spend, and the session would die at the four-hour mark.
	vault := authorized(t, refreshWindow/2)
	script := &scriptedTwitch{replies: []string{grantedBody}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	if _, err := session.Token(context.Background()); err != nil {
		t.Fatalf("Token() err = %v, want nil", err)
	}

	stored := storedSessionIn(t, vault)
	if stored.Refresh == "" {
		t.Error("the stored session carries no refresh token, so the next refresh has nothing to spend")
	}
	if stored.Access == "" {
		t.Error("the stored session carries no access token")
	}
	if stored.ExpiresAt.IsZero() {
		t.Error("the stored session carries no expiry, so every later call would refresh")
	}
}

func TestSession_ReportsARefusedRefreshTokenAsNeedingAuthorization(t *testing.T) {
	// Twitch expires a refresh token after about thirty days idle. The
	// remedy is the operator authorizing again, and nothing else, so it has
	// to be distinguishable from a network failure that will clear.
	vault := authorized(t, refreshWindow/2)
	script := &scriptedTwitch{replies: []string{`{"message":"Invalid refresh token"}`}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	if _, err := session.Token(context.Background()); !errors.Is(err, ErrReauthorize) {
		t.Errorf("Token() err = %v, want ErrReauthorize", err)
	}
}

func TestSession_SpendsTheRefreshTokenOnceUnderConcurrentCallers(t *testing.T) {
	// A refresh token is one time use. Two goroutines refreshing at once
	// spend it twice, and the second exchange is refused for a session that
	// is actually alive.
	vault := authorized(t, refreshWindow/2)
	script := &scriptedTwitch{replies: []string{grantedBody}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := session.Token(context.Background()); err != nil {
				t.Errorf("Token() err = %v, want nil", err)
			}
		})
	}
	wg.Wait()

	script.mu.Lock()
	defer script.mu.Unlock()
	if script.calls != 1 {
		t.Errorf("exchanged the refresh token %d times, want exactly 1", script.calls)
	}
}

func TestSession_RenewIgnoresAnExpiryThatStillLooksHealthy(t *testing.T) {
	// Renew is what a refusal from Helix calls. The operator can end the
	// authorization from Twitch's own settings, and the stored expiry says
	// nothing about that, so honouring it here would return the dead token.
	vault := authorized(t, 4*time.Hour)
	script := &scriptedTwitch{replies: []string{grantedBody}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	token, err := session.Renew(context.Background())
	if err != nil {
		t.Fatalf("Renew() err = %v, want nil", err)
	}
	if token == sentinelToken {
		t.Error("Renew() returned the stored token, want a freshly exchanged one")
	}
}

func TestSession_NoFailureCarriesAStoredToken(t *testing.T) {
	// An error is where a credential travels furthest: it gets wrapped,
	// logged, and pasted into a bug report.
	vault := authorized(t, refreshWindow/2)
	vault.setErr = errors.New("the disk is full")
	script := &scriptedTwitch{replies: []string{grantedBody}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	_, err := session.Token(context.Background())
	if err == nil {
		t.Fatal("Token() err = nil, want a failure")
	}
	for _, secret := range []string{sentinelToken, "REFRESHTOKENREFRESHTOKEN123456", vault.stored()} {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Errorf("the failure carries a credential: %q", err.Error())
		}
	}
}

func TestToken_GivesUpOnItsOwnDeadlineRatherThanTheHolders(t *testing.T) {
	// One session serves the recovery pass and the capture path both. A
	// caller waiting out somebody else's exchange is a stretch of a live
	// broadcast nobody is recording, and a mutex cannot be waited on with a
	// deadline, so the wait has to be abandonable.
	vault := &fakeVault{}
	session := NewSession(testClientID, vault, "test", nil)

	// Somebody else is mid-exchange and will not finish inside this test.
	session.hold()
	defer session.release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := session.Token(ctx)
	waited := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Token() err = %v, want the caller's deadline", err)
	}
	// Against the deadline rather than against the holder, which never
	// releases: a wait that outlives its own budget is the defect.
	if waited > 5*time.Second {
		t.Errorf("Token() waited %v, want it to give up at its deadline", waited)
	}
}

func TestRenew_GivesUpOnItsOwnDeadlineRatherThanTheHolders(t *testing.T) {
	vault := &fakeVault{}
	session := NewSession(testClientID, vault, "test", nil)

	session.hold()
	defer session.release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := session.Renew(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Renew() err = %v, want the caller's deadline", err)
	}
}

func TestSession_StillSpendsARefreshTokenOnce(t *testing.T) {
	// The serialization is the whole reason the gate exists. Making the wait
	// abandonable must not let two exchanges overlap, because a refresh
	// token is spent the moment one succeeds and the loser is told a live
	// session expired.
	vault := authorized(t, refreshWindow/2)
	// One grant, then a refusal. A second exchange reaching Twitch is the
	// defect, and answering it with a refusal is what makes that visible in
	// the count rather than only in a race detector.
	script := &scriptedTwitch{replies: []string{grantedBody, `{"error":"invalid_grant"}`}}
	session := NewSession(testClientID, vault, "test", script.serve(t))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			// Every caller has room to wait, so none abandons and the gate
			// is the only thing ordering them.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = session.Token(ctx)
		})
	}
	wg.Wait()

	if script.calls != 1 {
		t.Errorf("exchanged the refresh token %d times, want exactly 1", script.calls)
	}
}

func TestAuthorize_StoresWhatTheDeviceFlowGranted(t *testing.T) {
	vault := &fakeVault{}
	session := NewSession(testClientID, vault, "test", nil)

	if err := session.Authorize(Tokens{
		Access: sentinelToken, Refresh: "REFRESHTOKENREFRESHTOKEN123456", ExpiresIn: 4 * time.Hour,
	}); err != nil {
		t.Fatalf("Authorize() err = %v, want nil", err)
	}

	stored := storedSessionIn(t, vault)
	if stored.Access != sentinelToken || stored.Refresh == "" {
		t.Errorf("stored %+v, want both halves of the grant", stored)
	}
}

// ///////////////////////////////////////////////
// Videos
// ///////////////////////////////////////////////

func TestVideos_DeclinesAnAddressThatIsNotTwitch(t *testing.T) {
	// Discovery hands it whatever address the channel's provider built, so
	// a YouTube channel reaches this and must be declined rather than asked
	// about under a name Twitch would resolve to somebody else.
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, refusingClient(t))

	if _, err := helix.Videos(context.Background(), "https://youtube.com/@examplechannel", allArchives); err == nil {
		t.Error("Videos() err = nil for a non-Twitch address, want a refusal")
	}
}

// ///////////////////////////////////////////////
// Streams
// ///////////////////////////////////////////////

func TestStream_ReportsTheBroadcastOnAirWithoutALookup(t *testing.T) {
	// Get Streams takes the login, so the start of a broadcast in progress
	// costs one request. The Get Users lookup a listing needs is what this
	// deliberately skips.
	server := &helixServer{streams: `{"data":[{"id":"48211557693","user_login":"examplechannel",
		"started_at":"2026-03-01T20:00:00Z","title":"a broadcast",
		"game_name":"Just Chatting"}]}`}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	stream, live, err := helix.Stream(context.Background(), exampleChannel)
	if err != nil {
		t.Fatalf("Stream() err = %v, want nil", err)
	}
	if !live {
		t.Fatal("live = false, want the broadcast reported")
	}
	if server.userCalls != 0 {
		t.Errorf("asked for users %d times, want the lookup skipped", server.userCalls)
	}

	want := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	if !stream.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", stream.StartedAt, want)
	}
	if stream.ID != "48211557693" {
		t.Errorf("ID = %q, want the live session id", stream.ID)
	}
	if stream.Title != "a broadcast" || stream.Category != "Just Chatting" {
		t.Errorf("Stream() = %+v, want the title and category Twitch reported", stream)
	}
	if login := server.query("/streams").Get("user_login"); login != "examplechannel" {
		t.Errorf("user_login = %q, want the channel name", login)
	}
}

func TestStream_AnOfflineChannelIsAnAnswerRatherThanAFailure(t *testing.T) {
	// A caller that cannot tell an offline channel from a failed request
	// falls back for the wrong reason, and pays a second lookup every poll
	// of every channel that is simply not broadcasting.
	server := &helixServer{streams: `{"data":[]}`}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	stream, live, err := helix.Stream(context.Background(), exampleChannel)
	if err != nil {
		t.Fatalf("Stream() err = %v, want nil for an offline channel", err)
	}
	if live {
		t.Errorf("live = true with an empty answer, want false")
	}
	if !stream.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want the zero time", stream.StartedAt)
	}
}

func TestStream_RefusesAStartItCannotRead(t *testing.T) {
	// The value came off the network, so the failure must not repeat it:
	// an error is read in places a remote string must not reach.
	const hostile = "not-a-timestamp-<script>"
	server := &helixServer{streams: `{"data":[{"id":"1","user_login":"examplechannel","started_at":"` + hostile + `"}]}`}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	_, live, err := helix.Stream(context.Background(), exampleChannel)
	if err == nil {
		t.Fatal("Stream() err = nil, want an unreadable start refused")
	}
	if live {
		t.Error("live = true, want false when the start cannot be read")
	}
	if strings.Contains(err.Error(), hostile) {
		t.Errorf("the failure repeats what the network said: %q", err.Error())
	}
}

func TestStream_RenewsAndRetriesOnceOnARefusal(t *testing.T) {
	// A token can be refused before it expires, because the operator can end
	// the authorization from Twitch's own settings.
	server := &helixServer{
		refuseFirst: true,
		streams:     `{"data":[{"id":"1","user_login":"examplechannel","started_at":"2026-03-01T20:00:00Z"}]}`,
	}
	source := &fixedSource{token: sentinelToken, renewed: "RENEWEDTOKENRENEWEDTOKEN12345"}
	helix := NewHelix(testClientID, source, server.serve(t))

	if _, live, err := helix.Stream(context.Background(), exampleChannel); err != nil || !live {
		t.Fatalf("Stream() = %v, %v, want the retry to succeed", live, err)
	}
	if source.renewals != 1 {
		t.Errorf("renewals = %d, want exactly one", source.renewals)
	}
	if server.streamCalls != 2 {
		t.Errorf("stream requests = %d, want the refusal and the retry", server.streamCalls)
	}
}

func TestStream_RefusesAnAddressThatIsNotAChannel(t *testing.T) {
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, refusingClient(t))

	if _, _, err := helix.Stream(context.Background(), "https://example.com/whatever"); err == nil {
		t.Error("Stream() err = nil, want an address with no channel in it refused")
	}
}

// ///////////////////////////////////////////////
// Videos
// ///////////////////////////////////////////////

func TestVideos_ReadsAWholePageInOneRequest(t *testing.T) {
	// The reason this exists. A flat listing plus one lookup per broadcast
	// costs 1 + N, and a channel with years of history spends that every
	// pass.
	server := &helixServer{users: exampleUser, videos: examplePage}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	videos, err := helix.Videos(context.Background(), exampleArchive, allArchives)
	if err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}
	if len(videos) != 2 {
		t.Fatalf("Videos() returned %d, want both archives", len(videos))
	}
	if server.videoCalls != 1 {
		t.Errorf("asked for videos %d times, want 1", server.videoCalls)
	}

	want := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	if !videos[0].StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", videos[0].StartedAt, want)
	}
	if videos[0].Duration != 3*time.Hour+8*time.Minute+33*time.Second {
		t.Errorf("Duration = %v, want the length Twitch reported", videos[0].Duration)
	}
	if videos[0].ID != "2100001" || videos[0].Title != "a broadcast" {
		t.Errorf("Videos()[0] = %+v, want the first archive", videos[0])
	}
}

func TestVideos_ReadsTheStreamID(t *testing.T) {
	// The stream id is the only thing that joins a stored copy to the row the
	// recorder opened while the channel was on air. Dropping it files every
	// live-captured broadcast a second time.
	tests := []struct {
		name  string
		page  string
		want  string
		title string
	}{
		{
			name:  "twitch reports the live session",
			page:  examplePage,
			want:  "48211557693",
			title: "a broadcast",
		},
		{
			name: "an archive with no session reported",
			page: `{"data":[
				{"id":"2100003","title":"orphaned","url":"https://www.twitch.tv/videos/2100003",
				 "created_at":"2026-03-01T20:00:00Z","duration":"1h"}
			]}`,
			want:  "",
			title: "orphaned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &helixServer{users: exampleUser, videos: tt.page}
			helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

			videos, err := helix.Videos(context.Background(), exampleArchive, allArchives)
			if err != nil {
				t.Fatalf("Videos() err = %v, want nil", err)
			}
			if len(videos) == 0 {
				t.Fatal("Videos() returned nothing, want the fixture archive")
			}
			if videos[0].Title != tt.title {
				t.Fatalf("Videos()[0].Title = %q, want %q", videos[0].Title, tt.title)
			}
			if videos[0].StreamID != tt.want {
				t.Errorf("StreamID = %q, want %q", videos[0].StreamID, tt.want)
			}
		})
	}
}

func TestVideos_ReadsMutedSegments(t *testing.T) {
	// Twitch replaces the audio of a stretch it judges to hold copyrighted
	// music, and says so on the answer this call already makes. Reading it
	// costs no extra request and it is the only warning that a hole patched
	// from the stored copy would come back silent.
	tests := []struct {
		name  string
		page  string
		want  []MutedSpan
		known bool
	}{
		{
			name: "two muted stretches",
			page: `{"data":[
				{"id":"2100001","title":"a broadcast","url":"https://www.twitch.tv/videos/2100001",
				 "created_at":"2026-03-01T20:00:00Z","duration":"3h8m33s",
				 "muted_segments":[{"duration":30,"offset":120},{"duration":180,"offset":7200}]}
			]}`,
			want: []MutedSpan{
				{Offset: 120 * time.Second, Duration: 30 * time.Second},
				{Offset: 7200 * time.Second, Duration: 180 * time.Second},
			},
			known: true,
		},
		{
			name: "twitch muted nothing",
			page: `{"data":[
				{"id":"2100001","title":"a broadcast","url":"https://www.twitch.tv/videos/2100001",
				 "created_at":"2026-03-01T20:00:00Z","duration":"3h8m33s","muted_segments":null}
			]}`,
			want:  []MutedSpan{},
			known: true,
		},
		{
			name: "an empty list means the same thing",
			page: `{"data":[
				{"id":"2100001","title":"a broadcast","url":"https://www.twitch.tv/videos/2100001",
				 "created_at":"2026-03-01T20:00:00Z","duration":"3h8m33s","muted_segments":[]}
			]}`,
			want:  []MutedSpan{},
			known: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &helixServer{users: exampleUser, videos: tt.page}
			helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

			videos, err := helix.Videos(context.Background(), exampleArchive, allArchives)
			if err != nil {
				t.Fatalf("Videos() err = %v, want nil", err)
			}
			if len(videos) != 1 {
				t.Fatalf("Videos() returned %d, want the fixture archive", len(videos))
			}

			// Twitch answered, so "nothing is muted" is a fact rather than an
			// absence, and a non-nil empty list is what carries that.
			if tt.known && videos[0].Muted == nil {
				t.Fatal("Muted = nil, want an answer Twitch gave")
			}
			if !slices.Equal(videos[0].Muted, tt.want) {
				t.Errorf("Muted = %+v, want %+v", videos[0].Muted, tt.want)
			}
		})
	}
}

func TestVideos_AsksOnlyForArchives(t *testing.T) {
	// Highlights and uploads are edits of a broadcast, not the broadcast.
	// Recording one as a past broadcast would file a clip on the calendar as
	// though the recorder had missed a whole session.
	server := &helixServer{users: exampleUser, videos: examplePage}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}
	if got := server.query("/videos").Get("type"); got != "archive" {
		t.Errorf("type = %q, want archive", got)
	}
}

func TestVideos_AsksForTheLargestPageTwitchWillGive(t *testing.T) {
	// A page is one request either way, so a smaller one only means more of
	// them to reach the same horizon. Twitch refuses a page past its own
	// maximum, which is what bounds this.
	server := &helixServer{users: exampleUser, videos: examplePage}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}
	if got, want := server.query("/videos").Get("first"), strconv.Itoa(maxVideos); got != want {
		t.Errorf("first = %q, want %q", got, want)
	}
}

func TestVideos_PagesUntilTheHorizon(t *testing.T) {
	// A fixed count of the newest archives is a horizon measured in
	// broadcasts rather than in days. On a channel that streams daily it
	// reaches about a week, so a machine off for a fortnight comes back,
	// describes the newest page, and never learns the older broadcasts
	// happened while the archive still holds them.
	server := &helixServer{users: exampleUser, pages: []string{firstPage, secondPage}}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	videos, err := helix.Videos(context.Background(), exampleArchive,
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}

	if len(videos) != 2 {
		t.Fatalf("Videos() returned %d, want both pages walked", len(videos))
	}
	if server.videoCalls != 2 {
		t.Errorf("made %d video requests, want 2: the first page carried a cursor", server.videoCalls)
	}
	if got := server.query("/videos").Get("after"); got != "cursor-1" {
		t.Errorf("after = %q, want the cursor the first page returned", got)
	}
}

func TestVideos_StopsPagingOnceThePageReachesPastTheHorizon(t *testing.T) {
	// The horizon is the bound, so a page that already reaches past it is
	// the last one however many cursors Twitch keeps offering.
	server := &helixServer{users: exampleUser, pages: []string{firstPage, secondPage}}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive,
		time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}
	if server.videoCalls != 1 {
		t.Errorf("made %d video requests, want 1: the first page already reached past the horizon",
			server.videoCalls)
	}
}

func TestVideos_StopsAtThePageCapWhenTheCursorNeverEnds(t *testing.T) {
	// A cursor that never terminates, from a bug at either end, would walk a
	// channel's whole history on every pass.
	server := &helixServer{users: exampleUser, pages: []string{endlessPage}}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}
	if server.videoCalls != maxVideoPages {
		t.Errorf("made %d video requests, want the cap of %d", server.videoCalls, maxVideoPages)
	}
}

func TestVideos_DropsABroadcastWithNoReadableStart(t *testing.T) {
	// The calendar buckets by start time, so dating one now would file the
	// broadcast on whatever day the daemon happened to look, for good.
	server := &helixServer{
		users: exampleUser,
		videos: `{"data":[
			{"id":"2100001","title":"undated","url":"https://www.twitch.tv/videos/2100001","duration":"1h"},
			{"id":"2100002","title":"dated","url":"https://www.twitch.tv/videos/2100002",
			 "created_at":"2026-03-01T20:00:00Z","duration":"1h"}
		]}`,
	}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	videos, err := helix.Videos(context.Background(), exampleArchive, allArchives)
	if err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}
	if len(videos) != 1 || videos[0].ID != "2100002" {
		t.Errorf("Videos() = %+v, want only the dated broadcast", videos)
	}
}

func TestVideos_CarriesTheTokenInAHeaderAndNotTheAddress(t *testing.T) {
	// A token in a URL reaches a proxy log, a browser history, and this
	// process's own error strings.
	server := &helixServer{users: exampleUser, videos: examplePage}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err != nil {
		t.Fatalf("Videos() err = %v, want nil", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	for _, seen := range server.seen {
		if strings.Contains(seen.String(), sentinelToken) {
			t.Errorf("the token reached the address: %q", seen.String())
		}
	}
	for _, header := range server.auth {
		if header != "Bearer "+sentinelToken {
			t.Errorf("Authorization = %q, want the bearer token", header)
		}
	}
}

func TestVideos_RenewsAndRetriesOnceWhenTwitchRefusesTheToken(t *testing.T) {
	// A refusal is not only an expired token: the operator can end the
	// authorization from Twitch's settings, and the stored expiry says
	// nothing about that.
	server := &helixServer{users: exampleUser, videos: examplePage, refuseFirst: true}
	source := &fixedSource{token: sentinelToken, renewed: "FRESHTOKENFRESHTOKEN123456789"}
	helix := NewHelix(testClientID, source, server.serve(t))

	videos, err := helix.Videos(context.Background(), exampleArchive, allArchives)
	if err != nil {
		t.Fatalf("Videos() err = %v, want the retry to succeed", err)
	}
	if len(videos) != 2 {
		t.Errorf("Videos() returned %d after the retry, want both archives", len(videos))
	}
	if source.renewals != 1 {
		t.Errorf("renewed %d times, want exactly 1", source.renewals)
	}
	if last := server.auth[len(server.auth)-1]; last != "Bearer "+source.renewed {
		t.Errorf("the retry sent %q, want the renewed token", last)
	}
}

func TestVideos_ReportsASessionThatCannotBeRenewed(t *testing.T) {
	// The one outcome an operator has to act on, so it must not be buried
	// under a message about a request that failed.
	server := &helixServer{users: exampleUser, videos: examplePage, refuseFirst: true}
	source := &fixedSource{token: sentinelToken, renewErr: ErrReauthorize}
	helix := NewHelix(testClientID, source, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); !errors.Is(err, ErrReauthorize) {
		t.Errorf("Videos() err = %v, want ErrReauthorize", err)
	}
}

func TestVideos_ReportsAMachineWithNothingAuthorized(t *testing.T) {
	helix := NewHelix(testClientID, &fixedSource{tokenErr: ErrNoSession}, refusingClient(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); !errors.Is(err, ErrNoSession) {
		t.Errorf("Videos() err = %v, want ErrNoSession", err)
	}
}

func TestVideos_LooksUpTheChannelIdOnlyOnce(t *testing.T) {
	// An account's numeric id never changes, so asking every pass spends a
	// request per channel forever for an answer that is already known.
	server := &helixServer{users: exampleUser, videos: examplePage}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	for range 3 {
		if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err != nil {
			t.Fatalf("Videos() err = %v, want nil", err)
		}
	}
	if server.userCalls != 1 {
		t.Errorf("resolved the channel id %d times, want 1", server.userCalls)
	}
}

func TestVideos_ReportsAChannelTwitchDoesNotKnow(t *testing.T) {
	// A misspelled channel in the config would otherwise list somebody
	// else's broadcasts, or nobody's, without saying which.
	server := &helixServer{users: `{"data":[]}`, videos: examplePage}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err == nil {
		t.Error("Videos() err = nil for an unknown channel, want a refusal")
	}
}

// ///////////////////////////////////////////////
// Reading Twitch's shapes
// ///////////////////////////////////////////////

func TestLoginFromURL_ReadsWhatTheProviderBuilds(t *testing.T) {
	// It is the inverse of LiveURL and ArchiveURL. A form those two produce
	// that this cannot read is a channel silently left to the slow path.
	cases := []struct {
		name  string
		given string
		want  string
		ok    bool
	}{
		{name: "the live address", given: Provider{}.LiveURL("examplechannel"), want: "examplechannel", ok: true},
		{name: "the archive address", given: Provider{}.ArchiveURL("examplechannel"), want: "examplechannel", ok: true},
		{name: "with the www host", given: "https://www.twitch.tv/examplechannel", want: "examplechannel", ok: true},
		{name: "with a trailing slash", given: "https://twitch.tv/examplechannel/", want: "examplechannel", ok: true},
		{name: "another platform", given: "https://youtube.com/@examplechannel"},
		{name: "a host that merely ends in twitch.tv", given: "https://nottwitch.tv/examplechannel"},
		{name: "no channel at all", given: "https://twitch.tv/"},
		{name: "not an address", given: "://"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := loginFromURL(testCase.given)
			if ok != testCase.ok {
				t.Fatalf("loginFromURL(%q) ok = %v, want %v", testCase.given, ok, testCase.ok)
			}
			if got != testCase.want {
				t.Errorf("loginFromURL(%q) = %q, want %q", testCase.given, got, testCase.want)
			}
		})
	}
}

func TestReadDuration_ReadsTwitchsSpelling(t *testing.T) {
	// A wrong duration sizes a gap wrongly, and backfill patches what the
	// gap says is missing.
	cases := []struct {
		name  string
		given string
		want  time.Duration
	}{
		{name: "hours minutes seconds", given: "3h8m33s", want: 3*time.Hour + 8*time.Minute + 33*time.Second},
		{name: "minutes and seconds", given: "55m2s", want: 55*time.Minute + 2*time.Second},
		{name: "seconds alone", given: "44s", want: 44 * time.Second},
		{name: "unreported", given: ""},
		{name: "a form this cannot read", given: "about three hours"},
		{name: "negative", given: "-1h"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := readDuration(testCase.given); got != testCase.want {
				t.Errorf("readDuration(%q) = %v, want %v", testCase.given, got, testCase.want)
			}
		})
	}
}

func TestStream_RefusesAReplyThatOmitsTheChannelItDescribes(t *testing.T) {
	// Letting an absent user_login pass makes the guard something the reply
	// can switch off by leaving a field out. A broadcast attributed to the
	// wrong channel is filed under that channel, with its title and its
	// start, and every later listing reads it as that channel's.
	server := &helixServer{streams: `{"data":[{"id":"1","started_at":"2026-03-01T20:00:00Z"}]}`}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, _, err := helix.Stream(context.Background(), exampleChannel); err == nil {
		t.Error("Stream() accepted a reply naming no channel, want a refusal")
	}
}

func TestStream_RefusesAReplyAboutAnotherChannel(t *testing.T) {
	server := &helixServer{streams: `{"data":[{"id":"1","user_login":"someoneelse",` +
		`"started_at":"2026-03-01T20:00:00Z"}]}`}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, _, err := helix.Stream(context.Background(), exampleChannel); err == nil {
		t.Error("Stream() accepted a reply about another channel, want a refusal")
	}
}

func TestVideos_RefusesAUserLookupAboutAnotherChannel(t *testing.T) {
	// The id is cached for the life of the process, so a wrong one is read
	// by every later listing for that channel: the operator's calendar then
	// fills with somebody else's broadcasts.
	server := &helixServer{
		users:  `{"data":[{"id":"999","login":"someoneelse"}]}`,
		videos: examplePage,
	}
	helix := NewHelix(testClientID, &fixedSource{token: sentinelToken}, server.serve(t))

	if _, err := helix.Videos(context.Background(), exampleArchive, allArchives); err == nil {
		t.Error("Videos() accepted a user lookup about another channel, want a refusal")
	}
}
