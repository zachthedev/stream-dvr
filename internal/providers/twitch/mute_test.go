package twitch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// cdn serves a playlist directory, answering the segment probe with a
// chosen status.
type cdn struct {
	playlist       string
	playlistStatus int
	segmentStatus  int
	// refuse names segments this copy no longer holds the original of, so a
	// test can spell out storage that kept some and lost others.
	refuse map[string]bool

	// mu guards segmentsAsked, which the handler writes on its own goroutine
	// and the test reads on another.
	mu sync.Mutex
	// segmentsAsked records every segment path probed, so a test can prove
	// exactly which segments a set of spans asks about.
	segmentsAsked []string
}

// mutedPlaylist is what index-dvr.m3u8 holds for a copy with one silenced
// stretch, shaped the way the CDN serves it: CRLF endings, bare segment
// names, and the silenced ones renamed.
const mutedPlaylist = "#EXTM3U\r\n" +
	"#EXT-X-VERSION:6\r\n" +
	"#EXTINF:10.000,\r\n" +
	"16.mp4\r\n" +
	"#EXTINF:10.000,\r\n" +
	"17-unmuted.mp4\r\n" +
	"#EXTINF:10.000,\r\n" +
	"18-unmuted.mp4\r\n"

// probeSpans covers both silenced stretches mutedPlaylist names, which sit
// at ten and twenty seconds in.
var probeSpans = []Span{{Offset: 10 * time.Second, Duration: 20 * time.Second}}

// serve returns a client and the address of a media playlist on it.
//
// TLS because the probe refuses anything else, which is the guard keeping a
// remote playlist from moving a request onto another scheme.
func (c *cdn) serve(t *testing.T) (*http.Client, string) {
	t.Helper()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			if c.playlistStatus != 0 && c.playlistStatus != http.StatusOK {
				w.WriteHeader(c.playlistStatus)
				return
			}
			_, _ = w.Write([]byte(c.playlist))
			return
		}
		c.mu.Lock()
		c.segmentsAsked = append(c.segmentsAsked, r.URL.Path)
		c.mu.Unlock()

		if c.refuse[path.Base(r.URL.Path)] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(c.segmentStatus)
	}))
	t.Cleanup(server.Close)

	return server.Client(), server.URL + "/vod/chunked/index-muted-ABC123.m3u8"
}

// asked returns the segments probed, newest last.
func (c *cdn) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.segmentsAsked...)
}

// ///////////////////////////////////////////////
// OriginalAudio
// ///////////////////////////////////////////////

func TestOriginalAudio_ReportsACopyThatStillHoldsIt(t *testing.T) {
	// Twitch's newer storage keeps the audio as broadcast beside the
	// silenced variant, so a hole over a silenced stretch is patchable
	// rather than permanent.
	server := &cdn{playlist: mutedPlaylist, segmentStatus: http.StatusOK}
	client, playlistURL := server.serve(t)

	original, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
	if err != nil {
		t.Fatalf("OriginalAudio() err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("ok = false, want the copy reported recoverable")
	}
	if !strings.HasSuffix(original, "/vod/chunked/"+originalPlaylist) {
		t.Errorf("playlist = %q, want the sibling holding the original audio", original)
	}
	// Every segment the spans cover, not a sample. One answer generalised to
	// the rest is what lets a copy with mixed storage through.
	asked := server.asked()
	if len(asked) != 2 {
		t.Fatalf("probed %v, want both silenced segments", asked)
	}
	for i, want := range []string{"/17-unmuted.mp4", "/18-unmuted.mp4"} {
		if !strings.HasSuffix(asked[i], want) {
			t.Errorf("probe %d = %q, want %q", i, asked[i], want)
		}
	}
}

func TestOriginalAudio_RefusesACopyHoldingOnlySomeOfTheOriginals(t *testing.T) {
	// The case a single probe cannot see. Storage kept the first silenced
	// stretch and lost the second, so a lookup that asked about one and
	// generalised would authorise a download whose second stretch comes back
	// silent. The hole would then be closed permanently over nothing, which
	// is worse than never patching it.
	server := &cdn{
		playlist:      mutedPlaylist,
		segmentStatus: http.StatusOK,
		refuse:        map[string]bool{"18-unmuted.mp4": true},
	}
	client, playlistURL := server.serve(t)

	_, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
	if err != nil {
		t.Fatalf("OriginalAudio() err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true with one original missing, want the whole copy refused")
	}
}

func TestOriginalAudio_AsksOnlyAboutTheStretchesTheHolesCover(t *testing.T) {
	// A broadcast may be silenced in places no hole touches. Probing those
	// would refuse a recovery over audio nothing was going to fetch, and
	// spend a request per segment doing it.
	server := &cdn{playlist: mutedPlaylist, segmentStatus: http.StatusOK}
	client, playlistURL := server.serve(t)

	// The first silenced segment only, which runs from ten seconds in.
	spans := []Span{{Offset: 10 * time.Second, Duration: 5 * time.Second}}

	if _, ok, err := OriginalAudio(context.Background(), client, playlistURL, spans); err != nil || !ok {
		t.Fatalf("OriginalAudio() = %v, %v, want it recoverable", ok, err)
	}
	asked := server.asked()
	if len(asked) != 1 || !strings.HasSuffix(asked[0], "/17-unmuted.mp4") {
		t.Errorf("probed %v, want only the segment the span covers", asked)
	}
}

func TestOriginalAudio_NoSpansAsksNothing(t *testing.T) {
	// A broadcast whose holes overlap nothing silenced has no question to
	// ask, and asking would cost a playlist read per broadcast in a library
	// where most copies are not silenced at all.
	server := &cdn{playlist: mutedPlaylist, segmentStatus: http.StatusOK}
	client, playlistURL := server.serve(t)

	if _, ok, err := OriginalAudio(context.Background(), client, playlistURL, nil); err != nil || ok {
		t.Errorf("OriginalAudio() = %v, %v, want false with no error", ok, err)
	}
	if asked := server.asked(); len(asked) != 0 {
		t.Errorf("probed %v, want nothing", asked)
	}
}

func TestOriginalAudio_RefusesACopyStoredBeforeTwitchKeptOriginals(t *testing.T) {
	// The ordinary case, and the one the whole guard exists for. Older
	// storage destroyed the original, so the segment is named in the
	// playlist and serves nothing. Patching anyway is what returns footage
	// from elsewhere in the broadcast at exactly the right length.
	server := &cdn{playlist: mutedPlaylist, segmentStatus: http.StatusForbidden}
	client, playlistURL := server.serve(t)

	_, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
	if err != nil {
		t.Fatalf("OriginalAudio() err = %v, want a refusal reported as an answer", err)
	}
	if ok {
		t.Error("ok = true, want false when the original does not serve")
	}
}

func TestOriginalAudio_ACopyWithNothingSilencedIsNotAFailure(t *testing.T) {
	server := &cdn{
		playlist:      "#EXTM3U\r\n#EXTINF:10.000,\r\n16.mp4\r\n",
		segmentStatus: http.StatusOK,
	}
	client, playlistURL := server.serve(t)

	_, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
	if err != nil {
		t.Fatalf("OriginalAudio() err = %v, want nil for a copy with nothing silenced", err)
	}
	if ok {
		t.Error("ok = true, want false when there is nothing to recover")
	}
	if asked := server.asked(); len(asked) != 0 {
		t.Errorf("probed %d segments, want none when nothing is silenced", len(asked))
	}
}

func TestOriginalAudio_RefusesASegmentNameReachingOutsideTheDirectory(t *testing.T) {
	// The playlist is a remote document. A name carrying a path would make
	// the probe, and then the download, reason about a directory this never
	// checked.
	server := &cdn{
		playlist: "#EXTM3U\r\n#EXTINF:10.000,\r\n" +
			"../../elsewhere/9-unmuted.mp4\r\n" +
			"https://example.invalid/9-unmuted.mp4\r\n",
		segmentStatus: http.StatusOK,
	}
	client, playlistURL := server.serve(t)

	_, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
	if err != nil {
		t.Fatalf("OriginalAudio() err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true, want a name outside the directory ignored")
	}
	if asked := server.asked(); len(asked) != 0 {
		t.Errorf("probed %v, want nothing outside the playlist's own directory", asked)
	}
}

func TestOriginalAudio_TellsASettledRefusalFromABadMoment(t *testing.T) {
	// The two answers cost very different things. A copy stored before
	// Twitch kept originals has no such playlist at all, and reading that as
	// "ask later" spends a subprocess and a request on every pass for the
	// life of the library without the hole ever retiring. A gateway having a
	// bad minute is the opposite: reporting it as settled retires a hole the
	// platform would have served.
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "the copy has no such playlist", status: http.StatusNotFound},
		{name: "the edge refuses it outright", status: http.StatusForbidden},
		{name: "the copy is gone", status: http.StatusGone},
		{name: "the gateway is having a bad minute", status: http.StatusBadGateway, wantErr: true},
		{name: "the edge is rate limiting", status: http.StatusTooManyRequests, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &cdn{playlistStatus: tt.status}
			client, playlistURL := server.serve(t)

			_, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
			if ok {
				t.Error("ok = true, want false whatever the playlist answered")
			}
			if tt.wantErr && err == nil {
				t.Error("err = nil, want a transient answer reported so the next pass asks again")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("err = %v, want a settled refusal reported as an answer", err)
			}
		})
	}
}

func TestOriginalAudio_ASegmentTheEdgeWillNotAnswerIsARefusal(t *testing.T) {
	// A method the edge declines is its settled position rather than a
	// failure, and treating it as one leaves the hole unanswered and
	// uncharged on every pass forever.
	server := &cdn{playlist: mutedPlaylist, segmentStatus: http.StatusMethodNotAllowed}
	client, playlistURL := server.serve(t)

	_, ok, err := OriginalAudio(context.Background(), client, playlistURL, probeSpans)
	if err != nil {
		t.Fatalf("OriginalAudio() err = %v, want the refusal reported as an answer", err)
	}
	if ok {
		t.Error("ok = true, want false when the edge will not answer for the segment")
	}
}
