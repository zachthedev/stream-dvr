package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Vault is where an authorized session is kept between runs.
//
// Declared here at the point of use. This package holds no opinion about
// which store is behind it, and names no account of its own. The caller
// supplies both, which keeps every account name in one place.
type Vault interface {
	Get(account string) (string, error)
	Set(account, value string) error
}

// TokenSource supplies the user token Helix is called with.
//
// Renew is separate from Token because a token can be refused before it
// expires. The operator can end the authorization from Twitch's own
// settings, and only the refusal says so.
type TokenSource interface {
	// Token returns an access token, refreshing one near expiry.
	Token(ctx context.Context) (string, error)
	// Renew spends the refresh token whatever the stored expiry claims.
	Renew(ctx context.Context) (string, error)
}

// Session keeps a device-flow token pair alive across runs.
//
// A public client has no secret, so Twitch issues no application token and
// every Helix call carries a user token. Refreshing is load bearing here,
// not a convenience. The operator authorizes once, and a session that cannot
// refresh sends them back to authorize again.
type Session struct {
	vault   Vault
	account string
	client  *http.Client

	// clientID names the Twitch application this install acts as. It comes
	// from config, so every install authenticates as its own registration.
	clientID string

	// now is the clock the expiry is measured against. A field so a test
	// places a token either side of the refresh window without waiting.
	now func() time.Time

	// gate serializes refresh. A refresh token is one time use, so two
	// goroutines refreshing at once spend it twice and the second gets a
	// refusal for a session that is actually alive.
	//
	// It is a one-slot channel rather than a mutex because a mutex cannot be
	// waited on with a deadline. One session serves both the recovery pass
	// and the capture path, and the capture path is bounded: a poll waiting
	// out somebody else's exchange is a stretch of a live broadcast nobody
	// is recording. A waiter that runs out of time gives up here instead.
	//
	// A zero Session has a nil channel, which blocks forever, so every
	// construction path has to fill it. NewSession is the only one.
	gate chan struct{}
}

// Helix reads broadcast metadata from Twitch's REST API.
type Helix struct {
	tokens TokenSource
	client *http.Client

	// clientID names the Twitch application every request identifies as.
	clientID string

	// mu guards logins.
	mu sync.Mutex

	// logins maps a channel name to the numeric id Get Videos wants, which
	// no address carries. An account's id never changes, so it is looked up
	// once per process rather than once per pass.
	logins map[string]string
}

// Video is one past broadcast as Helix describes it.
type Video struct {
	// ID is the video id, which is the remote id a broadcast row carries.
	ID string
	// StreamID is the live session this recording came from, which is the
	// same id streamlink reports while the channel is on air. It is what
	// joins a stored copy to the row the recorder opened.
	StreamID string
	// Title is the broadcast title as the streamer wrote it. Untrusted.
	Title string
	// URL is where the broadcast can be fetched from.
	URL string
	// StartedAt is when the broadcast began, from Twitch's own timestamp.
	StartedAt time.Time
	// Duration is how long it ran, or zero when Twitch worded it in a way
	// this cannot read.
	Duration time.Duration
	// Muted is the stretches Twitch replaced the audio of, against the
	// stored copy's own timeline.
	//
	// Nil means Twitch was not asked. An empty list means it answered and
	// muted nothing, which is a fact rather than an absence, and the two must
	// not be confused: only the second one licenses patching a hole from this
	// copy.
	Muted []MutedSpan
}

// MutedSpan is one stretch of a stored copy whose audio the platform
// replaced with silence.
//
// Playback serves silence for these, so a patch taken the ordinary way
// fills a hole with nothing. Whether the audio as broadcast survives beside
// it depends on how the copy was stored, and OriginalAudio is what asks.
// This is what says the question is worth asking at all.
type MutedSpan struct {
	// Offset is where the stretch begins in the stored copy.
	Offset time.Duration
	// Duration is how long it runs.
	Duration time.Duration
}

// Stream is a broadcast Twitch reports as on air now.
//
// It answers the one question no archive can: when a broadcast began on a
// channel that publishes no VOD at all. Such a channel has nothing for a
// listing to describe, so this is the only route to its true start.
type Stream struct {
	// ID is the live session id, which is the same id streamlink reports
	// while the channel is on air.
	ID string
	// StartedAt is when the broadcast began, from Twitch's own timestamp.
	StartedAt time.Time
	// Title is the broadcast title as the streamer wrote it. Untrusted.
	Title string
	// Category is the game or section the broadcast is filed under.
	Category string
}

// storedSession is how a token pair is written to the vault.
type storedSession struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	// ExpiresAt is when the access token dies, so a process starting hours
	// later knows without spending a request to find out.
	ExpiresAt time.Time `json:"expires_at"`
}

// videoPage is Helix's answer to Get Videos.
type videoPage struct {
	Data []struct {
		ID        string `json:"id"`
		StreamID  string `json:"stream_id"`
		Title     string `json:"title"`
		URL       string `json:"url"`
		CreatedAt string `json:"created_at"`
		Duration  string `json:"duration"`
		// MutedSegments is null when nothing is muted, which decodes to a nil
		// slice and is indistinguishable from an absent field. Videos answers
		// the distinction instead: this call was made, so Twitch answered.
		MutedSegments []struct {
			Offset   int64 `json:"offset"`
			Duration int64 `json:"duration"`
		} `json:"muted_segments"`
	} `json:"data"`
	// Pagination carries the cursor for the next page. It is absent on the
	// last one, which is how paging knows to stop.
	Pagination struct {
		Cursor string `json:"cursor"`
	} `json:"pagination"`
}

// userPage is Helix's answer to Get Users.
type userPage struct {
	Data []struct {
		ID string `json:"id"`
		// Login is decoded so the answer can be checked against the
		// question. Without it the reply cannot be cross-checked even in
		// principle, and a wrong id is cached for the life of the process:
		// every later listing for that channel then reads another
		// account's archive.
		Login string `json:"login"`
	} `json:"data"`
}

// streamPage is Helix's answer to Get Streams.
//
// An offline channel is an empty Data rather than an error, which is how
// absence is told apart from a request that failed.
type streamPage struct {
	Data []struct {
		ID        string `json:"id"`
		UserLogin string `json:"user_login"`
		StartedAt string `json:"started_at"`
		Title     string `json:"title"`
		GameName  string `json:"game_name"`
	} `json:"data"`
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// helixURL is where the REST API lives.
const helixURL = "https://api.twitch.tv/helix"

// refreshWindow is how much life an access token must have left to be handed
// out as it is.
//
// A token that expires mid-pass costs a refusal and a retry on every request
// after it. Renewing while it still has minutes on it costs one request the
// pass makes anyway.
const refreshWindow = 15 * time.Minute

// maxVideos is the largest page Get Videos will return.
const maxVideos = 100

// maxVideoPages bounds how many pages one listing walks.
//
// The horizon normally stops paging long before this. The cap is what keeps
// a cursor that never terminates, from a bug at either end, from walking a
// channel's whole history on every pass.
const maxVideoPages = 10

// maxMutedSeconds bounds an offset or a length in a silenced stretch.
//
// A year, which no broadcast approaches and which leaves the conversion to
// nanoseconds four orders of magnitude short of overflowing. The bound
// exists for the arithmetic rather than for the platform: a span whose end
// wraps negative reads as covering nothing, which turns the refusal to patch
// silenced audio into a permanent fill with silence.
const maxMutedSeconds = 365 * 24 * 60 * 60

// maxHelixResponse bounds what is read from a Helix reply.
//
// A full page is a hundred broadcasts, each a few hundred bytes of title and
// timestamps. The bound is what stops a wrong endpoint, or a captive portal,
// being read into memory without limit.
const maxHelixResponse = 1 << 20

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

var (
	// ErrNoSession reports a machine with no authorized metadata session. It
	// is an ordinary state rather than a fault: the API is an optimisation
	// and discovery works without it.
	ErrNoSession = errors.New("no twitch metadata session is authorized")

	// ErrReauthorize reports a refresh token Twitch will not honour. Twitch
	// expires one after about thirty days idle, so a daemon off that long
	// lands here and the operator authorizes once more.
	ErrReauthorize = errors.New("the twitch metadata session expired, so it must be authorized again")

	// errNotTwitch reports an address no channel name can be read out of.
	errNotTwitch = errors.New("not a twitch channel address")
)

// ///////////////////////////////////////////////
// Session
// ///////////////////////////////////////////////

// NewSession returns a session kept under account in vault.
func NewSession(clientID string, vault Vault, account string, client *http.Client) *Session {
	if client == nil {
		client = defaultClient()
	}
	return &Session{
		vault: vault, account: account, client: client, now: time.Now,
		clientID: clientID,
		gate:     make(chan struct{}, 1),
	}
}

// acquire takes the refresh gate, or gives up when ctx does.
//
// Giving up is the point. A caller that runs out of time here has a
// fallback, and holding it to somebody else's exchange spends a stretch of
// a live broadcast on a timestamp.
func (s *Session) acquire(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hold takes the refresh gate and waits however long that takes.
//
// It is for the paths with no deadline to keep: an interactive command
// storing a grant, and a status read. Neither stands in front of a capture.
func (s *Session) hold() {
	s.gate <- struct{}{}
}

// release returns the refresh gate.
func (s *Session) release() {
	<-s.gate
}

// Authorize stores the pair a device flow granted.
func (s *Session) Authorize(tokens Tokens) error {
	s.hold()
	defer s.release()

	return s.save(tokens)
}

// Stored reports when the kept access token expires, and whether one is
// kept at all.
//
// It reaches nothing: no Twitch request, and no refresh. That makes it safe
// for a status command, which must answer offline and must not have side
// effects. Token is the opposite. It spends the one-time refresh token when
// the access half is near expiry, so a read-only caller would rotate the
// session just by asking.
//
// A session that cannot be read answers false, because every way of failing
// to read one leaves the caller with nothing to use.
func (s *Session) Stored() (time.Time, bool) {
	s.hold()
	defer s.release()

	stored, err := s.load()
	if err != nil || stored.Access == "" {
		return time.Time{}, false
	}
	return stored.ExpiresAt, true
}

// Token implements TokenSource.
//
// Refreshing happens here rather than on a timer. An access token lives about
// four hours, and only a recovery pass reads one. A pass that runs renews what
// it needs. A daemon with nothing to recover spends no requests keeping a
// token it is not using.
func (s *Session) Token(ctx context.Context) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()

	stored, err := s.load()
	if err != nil {
		return "", err
	}
	if stored.Access != "" && s.now().Add(refreshWindow).Before(stored.ExpiresAt) {
		return stored.Access, nil
	}
	return s.renew(ctx, stored)
}

// Renew implements TokenSource.
func (s *Session) Renew(ctx context.Context) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()

	stored, err := s.load()
	if err != nil {
		return "", err
	}
	return s.renew(ctx, stored)
}

// renew exchanges the stored refresh token and writes the new pair.
//
// THE NEW PAIR IS STORED BEFORE THE ACCESS TOKEN IS RETURNED. A Twitch
// refresh token is spent the moment the exchange succeeds. A caller handed
// the new access token before the write would, on a crash, leave a session
// nothing can recover.
func (s *Session) renew(ctx context.Context, stored storedSession) (string, error) {
	if stored.Refresh == "" {
		return "", ErrNoSession
	}

	granted, err := Refresh(ctx, s.client, stored.Refresh, s.clientID)
	if errors.Is(err, ErrInvalidToken) {
		return "", ErrReauthorize
	}
	if err != nil {
		return "", err
	}

	if err := s.save(granted); err != nil {
		return "", err
	}
	return granted.Access, nil
}

// load reads the stored pair.
//
// Every way of failing to read one answers ErrNoSession. The distinction
// would change nothing a caller does. Metadata is an optimisation, and a
// recorder must not stop because a faster listing is out of reach. Nothing
// about the stored value reaches the error, because a stored session is two
// credentials.
func (s *Session) load() (storedSession, error) {
	body, err := s.vault.Get(s.account)
	if err != nil {
		return storedSession{}, fmt.Errorf("%w: %w", ErrNoSession, err)
	}

	var stored storedSession
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		return storedSession{}, fmt.Errorf("%w: the stored session cannot be read", ErrNoSession)
	}
	return stored, nil
}

// save writes a granted pair, stamped with when its access half dies.
func (s *Session) save(tokens Tokens) error {
	// A pair with no refresh token cannot renew itself, so writing one over
	// a stored pair that can ends the session at the next expiry with
	// nothing said. Refused here as well as filled in at the point of
	// decode, because this is the write that makes it permanent and the
	// token it would replace has already been spent at the provider.
	if tokens.Refresh == "" {
		if stored, err := s.load(); err == nil && stored.Refresh != "" {
			return errors.New("refusing to store a session with no refresh token over one that has it")
		}
	}

	body, err := json.Marshal(storedSession{
		Access:    tokens.Access,
		Refresh:   tokens.Refresh,
		ExpiresAt: s.now().Add(tokens.ExpiresIn),
	})
	if err != nil {
		return fmt.Errorf("encoding the session: %w", err)
	}

	if err := s.vault.Set(s.account, string(body)); err != nil {
		return fmt.Errorf("storing the session: %w", err)
	}
	return nil
}

// ///////////////////////////////////////////////
// Helix
// ///////////////////////////////////////////////

// NewHelix returns a client reading metadata through tokens.
func NewHelix(clientID string, tokens TokenSource, client *http.Client) *Helix {
	if client == nil {
		client = defaultClient()
	}
	return &Helix{clientID: clientID, tokens: tokens, client: client, logins: map[string]string{}}
}

// Videos returns a channel's past broadcasts, newest first, back to the
// horizon.
//
// One request covers a whole page, timestamps and durations included, where
// a flat listing plus a lookup per broadcast costs 1 + N. That is why this
// exists, and a channel with years of history is where it shows.
//
// It pages until a broadcast starts before since, because the bound a caller
// cares about is how far back it needs to see rather than how many of the
// newest it gets. A fixed count of the newest archives reaches a week on a
// channel that streams daily, so a machine off for a fortnight comes back
// and never learns the older broadcasts existed.
func (h *Helix) Videos(ctx context.Context, channelURL string, since time.Time) ([]Video, error) {
	login, ok := loginFromURL(channelURL)
	if !ok {
		return nil, errNotTwitch
	}

	userID, err := h.userID(ctx, login)
	if err != nil {
		return nil, err
	}

	var videos []Video
	cursor := ""
	for range maxVideoPages {
		page, err := h.videoPage(ctx, userID, cursor)
		if err != nil {
			return nil, err
		}

		found, reached := decodeVideos(page, since)
		videos = append(videos, found...)
		if reached || page.Pagination.Cursor == "" || len(page.Data) == 0 {
			break
		}
		cursor = page.Pagination.Cursor
	}
	return videos, nil
}

// Stream returns the broadcast a channel is on air with, and whether Twitch
// reports one at all.
//
// Get Streams states started_at outright, so nothing here waits for an
// archive to appear or works out whether one is still being written. It
// takes the login rather than the numeric id, so it costs one request and
// never the Get Users lookup a listing needs.
//
// An offline channel is false with a nil error. That is an answer, not a
// failure, and a caller that cannot tell the two apart falls back for the
// wrong reason.
func (h *Helix) Stream(ctx context.Context, channelURL string) (Stream, bool, error) {
	login, ok := loginFromURL(channelURL)
	if !ok {
		return Stream{}, false, errNotTwitch
	}

	var page streamPage
	if err := h.get(ctx, "/streams", url.Values{"user_login": {login}}, &page); err != nil {
		return Stream{}, false, err
	}
	if len(page.Data) == 0 {
		return Stream{}, false, nil
	}

	entry := page.Data[0]
	// The query named one login, so a reply about another is not an answer
	// to it. An absent user_login fails the check rather than passing it:
	// letting the responder decide whether it is compared makes the guard
	// something the reply can switch off, and a broadcast attributed to
	// the wrong channel is filed under it.
	if !strings.EqualFold(entry.UserLogin, login) {
		return Stream{}, false, errors.New("twitch answered about a different channel")
	}

	started, err := time.Parse(time.RFC3339, entry.StartedAt)
	if err != nil {
		// The value is not repeated. It came from the network, and an error
		// is read in places a remote string must not reach.
		return Stream{}, false, errors.New("twitch reported a start this cannot read")
	}
	return Stream{
		ID:        entry.ID,
		StartedAt: started.UTC(),
		Title:     entry.Title,
		Category:  entry.GameName,
	}, true, nil
}

// videoPage reads one page of a channel's archives.
func (h *Helix) videoPage(ctx context.Context, userID, cursor string) (videoPage, error) {
	query := url.Values{
		"user_id": {userID},
		// Archives are past broadcasts. Highlights and uploads are edits of
		// them, which are not what a recorder missed.
		"type":  {"archive"},
		"first": {strconv.Itoa(maxVideos)},
	}
	if cursor != "" {
		query.Set("after", cursor)
	}

	var page videoPage
	if err := h.get(ctx, "/videos", query, &page); err != nil {
		return videoPage{}, err
	}
	return page, nil
}

// decodeVideos turns one page into broadcasts, and reports whether the page
// reached back past the horizon.
//
// A video older than the horizon is still returned. It cost nothing to
// fetch, the caller's own window decides what it does with it, and dropping
// it here would put a second copy of that decision in the wrong package.
func decodeVideos(page videoPage, since time.Time) ([]Video, bool) {
	videos := make([]Video, 0, len(page.Data))
	reached := false

	for _, entry := range page.Data {
		started, err := time.Parse(time.RFC3339, entry.CreatedAt)
		if err != nil {
			// Dropped rather than dated now. The calendar buckets by start
			// time, so inventing one files the broadcast on the wrong day
			// for good, and a listing is not worth that.
			continue
		}
		// Non-nil however Twitch worded the field, because this request is
		// itself the evidence the question was put. A caller reads nil as
		// "nobody asked", and that is the one answer this path cannot give.
		muted := make([]MutedSpan, 0, len(entry.MutedSegments))
		for _, segment := range entry.MutedSegments {
			// Bounded before it becomes a Duration. Seconds multiplied into
			// nanoseconds overflows well inside what the field can carry,
			// and a span whose end wraps negative reads as covering nothing,
			// so a silenced stretch would be patched from the copy that
			// serves silence and marked done for good.
			if segment.Offset < 0 || segment.Duration <= 0 ||
				segment.Offset > maxMutedSeconds || segment.Duration > maxMutedSeconds {
				continue
			}
			muted = append(muted, MutedSpan{
				Offset:   time.Duration(segment.Offset) * time.Second,
				Duration: time.Duration(segment.Duration) * time.Second,
			})
		}

		if started.Before(since) {
			reached = true
		}
		videos = append(videos, Video{
			ID:        entry.ID,
			StreamID:  entry.StreamID,
			Title:     entry.Title,
			URL:       entry.URL,
			StartedAt: started.UTC(),
			Duration:  readDuration(entry.Duration),
			Muted:     muted,
		})
	}
	return videos, reached
}

// userID resolves a channel name to the numeric id Get Videos takes.
func (h *Helix) userID(ctx context.Context, login string) (string, error) {
	h.mu.Lock()
	cached, known := h.logins[login]
	h.mu.Unlock()
	if known {
		return cached, nil
	}

	var page userPage
	if err := h.get(ctx, "/users", url.Values{"login": {login}}, &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 || page.Data[0].ID == "" {
		// The name is not repeated. It is the operator's own configuration,
		// so they have it already, and an error is where text travels
		// furthest.
		return "", errors.New("twitch knows no channel by that name")
	}
	// The query named one login, so a reply describing another is not an
	// answer to it. Checked before the id is cached, because a wrong one
	// cached here is read by every later listing for that channel.
	if !strings.EqualFold(page.Data[0].Login, login) {
		return "", errors.New("twitch answered about a different channel")
	}

	h.mu.Lock()
	h.logins[login] = page.Data[0].ID
	h.mu.Unlock()
	return page.Data[0].ID, nil
}

// get performs one Helix request, retrying once through a fresh token.
//
// A refusal is not only an expired token. The operator can end the
// authorization from Twitch's own settings, and the stored expiry says
// nothing about that. One renewal and one retry is what separates a token
// that aged out from a session that is really gone.
func (h *Helix) get(ctx context.Context, path string, query url.Values, into any) error {
	token, err := h.tokens.Token(ctx)
	if err != nil {
		return err
	}

	status, body, err := h.do(ctx, path, query, token)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		if token, err = h.tokens.Renew(ctx); err != nil {
			return err
		}
		if status, body, err = h.do(ctx, path, query, token); err != nil {
			return err
		}
	}

	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrReauthorize
	default:
		// Twitch's own words are not repeated: a body from an unexpected
		// status is not known to be free of the request that produced it,
		// and that request carried a token.
		return fmt.Errorf("twitch answered %d", status)
	}

	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("reading twitch's answer: %w", err)
	}
	return nil
}

// do sends one request and returns its status and body.
//
// The token travels in a header and nowhere else. It is never placed in the
// URL, where it would reach a proxy log and this process's own errors.
func (h *Helix) do(ctx context.Context, path string, query url.Values, token string) (int, []byte, error) {
	// Refused here rather than left to the one caller that builds a Helix.
	// The invariant belongs to whatever sends the header, and a request
	// identifying as no application is one Twitch answers unhelpfully.
	if h.clientID == "" {
		return 0, nil, ErrNoClientID
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		helixURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("building the request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Client-Id", h.clientID)

	// G704: the address is helixURL, a package constant, plus a
	// path this file chooses and an encoded query. Nothing reaches it raw.
	response, err := h.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("asking twitch: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxHelixResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("reading twitch's answer: %w", err)
	}
	return response.StatusCode, body, nil
}

// ///////////////////////////////////////////////
// Reading Twitch's shapes
// ///////////////////////////////////////////////

// loginFromURL reads the channel name out of a Twitch address.
//
// It is the inverse of Provider.LiveURL and ArchiveURL. An address it cannot
// read is one those two did not build, which is how a channel on another
// platform is declined rather than asked about.
func loginFromURL(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www."); host != "twitch.tv" {
		return "", false
	}

	// The first segment is the channel. Anything after it names one of its
	// pages, which ArchiveURL appends and this drops.
	name, _, _ := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	if name == "" {
		return "", false
	}
	return name, true
}

// readDuration reads Twitch's spelling of a broadcast length.
//
// It is written the way Go writes one, so the standard parser takes it. A
// form it cannot read answers zero, which the store already treats as
// unknown, rather than a guess that would size a gap wrongly.
func readDuration(written string) time.Duration {
	parsed, err := time.ParseDuration(written)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}
