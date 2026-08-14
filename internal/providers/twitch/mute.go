package twitch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Span is a stretch of a stored copy's own timeline.
//
// It is the shape a caller states a silenced stretch in, and it is
// deliberately not MutedSpan: this package answers about the segments a
// stretch covers, and where the caller learned the stretch is its business.
type Span struct {
	// Offset is where the stretch begins in the stored copy.
	Offset time.Duration
	// Duration is how long it runs.
	Duration time.Duration
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// extInfPrefix opens the line stating a segment's length.
const extInfPrefix = "#EXTINF:"

// maxProbedSegments bounds how many segments one lookup will ask about.
//
// Twitch silences in three minute blocks of ten second segments, so a block
// is eighteen of them and a long hole spanning several blocks is still well
// inside this. Past it the answer is refused rather than paid for: the point
// of asking is one request per segment a patch actually covers.
const maxProbedSegments = 256

// maxSegmentSeconds is the longest an EXTINF value may state before the
// line is treated as unreadable.
//
// A media segment runs seconds, and a day is already several orders of
// magnitude past anything a playlist describes. The bound exists because
// the value is multiplied into nanoseconds: what matters is that it be far
// below where that product overflows, not that it be tight.
const maxSegmentSeconds = 24 * 60 * 60

// lookupTimeout bounds one whole lookup, every probe in it included.
//
// The per-request timeout bounds a request that hangs. This bounds a CDN
// that answers every one of them slowly, which would otherwise multiply by
// the segment count and hold the pass behind a single broadcast.
const lookupTimeout = 2 * time.Minute

// originalPlaylist is the playlist name holding a stored copy's original
// audio.
//
// Playback resolves to index-muted-<HASH>.m3u8, which names the silenced
// stretches N-muted. This one sits beside it in the same directory and names
// the same stretches N-unmuted, which are separate objects carrying the
// audio as broadcast.
const originalPlaylist = "index-dvr.m3u8"

// originalSuffix is what a silenced stretch is called in the playlist that
// holds the audio as broadcast.
const originalSuffix = "-unmuted"

// maxPlaylistBytes bounds a media playlist read.
//
// A long broadcast lists a segment per ten seconds, so a day of them is a
// few hundred kilobytes. The bound is what stops a wrong address, or a
// captive portal, being read into memory without limit.
const maxPlaylistBytes = 8 << 20

// errNoOriginal reports a copy with nothing this lookup can recover: either
// it names no silenced stretch at all, or it names more of them across the
// range than one lookup will verify.
//
// Both are settled statements about the copy rather than failures to reach
// one, and both mean the same thing to a caller: there is no route here, so
// stop asking.
var errNoOriginal = errors.New("the copy names no silenced stretch this can recover")

// errSettled reports a status that is the platform's position rather than a
// bad moment, so the lookup answers no instead of failing.
//
// It never leaves this package. A caller sees the settled answer as false
// with no error, which is the same shape as a copy that simply holds no
// original, because it means the same thing: stop asking.
var errSettled = errors.New("the copy does not hold the audio as broadcast")

// final reports whether a status is the edge's settled position rather than
// a bad moment.
func final(status int) bool {
	switch status {
	case http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusGone:
		return true
	default:
		return false
	}
}

// ///////////////////////////////////////////////
// Recovery
// ///////////////////////////////////////////////

// OriginalAudio reports whether the stretches named by spans can still be
// fetched with the audio as broadcast, and the playlist to fetch them from.
//
// Twitch's newer storage keeps the original beside the silenced variant and
// serves both. Its older storage did not, so most copies answer no and the
// caller refuses the patch exactly as it would have anyway. Measured against
// the CDN: the original object answers 200, and the plain name every older
// tool asks for answers 403 whatever the copy's age.
//
// EVERY SEGMENT COVERING THE SPANS IS PROBED, not a sample of them. The
// platform states where it silenced audio, so the segments that matter are
// known rather than guessed, and a copy whose storage kept some originals
// and lost others has to answer no. Probing one and generalising would
// authorise a download whose silenced part comes back silent anyway, and the
// hole would then be closed permanently over nothing.
//
// A copy with nothing silenced across those spans answers false with no
// error. There is nothing to recover, which is not a failure.
func OriginalAudio(ctx context.Context, client *http.Client, playlistURL string,
	spans []Span,
) (string, bool, error) {
	if client == nil {
		client = defaultClient()
	}
	if len(spans) == 0 {
		return "", false, nil
	}

	// The whole lookup is bounded, not just each request in it. A copy can
	// name hundreds of silenced segments, and a CDN that answers slowly
	// rather than not at all would otherwise hold one broadcast for hours
	// while the pass waits behind it.
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	original, err := siblingIn(playlistURL, originalPlaylist)
	if err != nil {
		return "", false, err
	}

	body, err := readPlaylist(ctx, client, original)
	if errors.Is(err, errSettled) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	segments, err := originalSegmentsOver(body, spans)
	if errors.Is(err, errNoOriginal) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	for _, segment := range segments {
		address, err := siblingIn(original, segment)
		if err != nil {
			return "", false, err
		}

		served, err := serves(ctx, client, address)
		if err != nil {
			return "", false, err
		}
		// One that does not serve settles it. The download covers the whole
		// stretch, so a single missing object is a silent patch.
		if !served {
			return "", false, nil
		}
	}
	return original, true, nil
}

// linkLocalHost reports a host that names a link-local address, which
// reaches a metadata service rather than a copy of a broadcast.
//
// A literal address only. A name that resolves to one would need a check at
// dial time, which is a different guard than this one. Loopback is not
// refused: a local address is where a test server lives, and the caller
// already gates the broadcast on a twitch.tv host.
func linkLocalHost(host string) bool {
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if address.Is4In6() {
		address = address.Unmap()
	}
	return address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast()
}

// siblingIn returns the address of name in the same directory as address.
//
// It goes through the parser rather than through string surgery: the scheme
// carries its own double slash, which a path join collapses, and any query
// the CDN signs the address with has to survive onto the sibling.
func siblingIn(address, name string) (string, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("reading the playlist address: %w", err)
	}
	// The scheme is checked here rather than at the call site because every
	// address this package fetches is built by this function, so this is the
	// one place all of them pass through. A sibling inherits the host, so
	// nothing derived from a playlist's own text can move the request to
	// another one.
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("the playlist address is not an https address")
	}
	// The playlist address arrives from the fetch tool, so it is remote
	// text like everything else here. The caller gates the broadcast on a
	// twitch.tv host, and this is the second line: an address naming a
	// link-local host reaches a metadata service rather than a copy of the
	// broadcast. Config refuses one for the webhook and names this reason,
	// so the two agree.
	if linkLocalHost(parsed.Hostname()) {
		return "", errors.New("the playlist address is a link-local address")
	}

	cut := strings.LastIndex(parsed.Path, "/")
	if cut < 0 {
		return "", errors.New("the playlist address names no directory")
	}

	sibling := *parsed
	sibling.Path = parsed.Path[:cut+1] + name
	return sibling.String(), nil
}

// originalSegmentsOver returns the original-audio segments covering any of
// the spans.
//
// The playlist states each segment's length, so a stretch of the copy's
// timeline maps onto the segments holding it by accumulating those lengths.
// A segment is a candidate only where it both overlaps a span and is named
// as a silenced stretch: a span the platform reported covers segments that
// were never renamed at its edges, and those hold their audio already.
//
// The count is bounded. A playlist naming more silenced segments than a
// patch could reasonably cover is refused rather than probed, because the
// point of asking is to spend one request per segment that matters, not to
// walk a whole broadcast.
func originalSegmentsOver(playlist string, spans []Span) ([]string, error) {
	var (
		segments []string
		at       time.Duration
		length   time.Duration
	)

	for line := range strings.SplitSeq(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if after, found := strings.CutPrefix(line, extInfPrefix); found {
				length = readSegmentLength(after)
			}
			continue
		}

		start, end := at, at+length
		at, length = end, 0

		// A segment name only. An absolute address would point outside the
		// directory this probe reasoned about.
		if strings.Contains(line, "/") || strings.Contains(line, "..") {
			continue
		}
		if !strings.Contains(line, originalSuffix) || !overlapsAny(spans, start, end) {
			continue
		}
		// Settled rather than failed. The count follows from the copy and
		// the stretches asked about, neither of which changes, so reporting
		// it as a failure would leave the holes unanswered and uncharged on
		// every pass forever.
		if len(segments) == maxProbedSegments {
			return nil, errNoOriginal
		}
		segments = append(segments, line)
	}

	if len(segments) == 0 {
		return nil, errNoOriginal
	}
	return segments, nil
}

// readSegmentLength reads a segment length from an EXTINF value, which is
// seconds followed by a comma and an optional title.
//
// An unreadable length reads as zero, which leaves the running offset where
// it was. That misplaces later segments rather than skipping the check, and
// a misplaced segment is still probed if it overlaps.
func readSegmentLength(value string) time.Duration {
	field, _, _ := strings.Cut(value, ",")

	seconds, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	// Bounded before the multiply, not after. The guards above test the
	// value the playlist stated; the overflow happens in the product,
	// which reaches +Inf from about 9.3e9 seconds and converts to the
	// most negative Duration there is. One such line ahead of the
	// silenced segments drags the running offset negative, no segment
	// overlaps anything, and the hole is marked unrecoverable for the
	// life of the library.
	if seconds > maxSegmentSeconds {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// overlapsAny reports whether a segment shares any of its length with a
// span.
func overlapsAny(spans []Span, start, end time.Duration) bool {
	for _, span := range spans {
		if span.Offset < end && start < span.Offset+span.Duration {
			return true
		}
	}
	return false
}

// readPlaylist fetches a media playlist.
func readPlaylist(ctx context.Context, client *http.Client, address string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", fmt.Errorf("building the playlist request: %w", err)
	}

	// G704: siblingIn built this address, so it is https and
	// carries the host of the playlist the platform itself resolved. A
	// segment name from the playlist body can only replace the last path
	// element, never the scheme or the host.
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("reading the playlist: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		// A copy stored before Twitch kept originals has no such playlist at
		// all, and that is a permanent answer rather than a request to try
		// later. Told apart here so a caller does not ask again every pass
		// for the life of the library.
		if final(response.StatusCode) {
			return "", fmt.Errorf("%w: the playlist answered %d", errSettled, response.StatusCode)
		}
		return "", fmt.Errorf("the playlist answered %d", response.StatusCode)
	}

	// One byte past the bound, so a document that ran over is refused rather
	// than parsed. A truncated playlist names none of the stretches past the
	// cut, and reading that as "nothing to recover" answers a question this
	// never got to ask.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPlaylistBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading the playlist: %w", err)
	}
	if len(body) > maxPlaylistBytes {
		return "", fmt.Errorf("the playlist is longer than %d bytes", maxPlaylistBytes)
	}
	return string(body), nil
}

// serves reports whether an address answers, without reading it.
//
// The question is whether the object exists, so the body is never fetched. A
// refusal is an answer rather than a failure: it is what a copy stored
// before Twitch kept originals looks like, and it is the ordinary case.
func serves(ctx context.Context, client *http.Client, address string) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, address, nil)
	if err != nil {
		return false, fmt.Errorf("building the segment request: %w", err)
	}

	// G704: as above. The address came from siblingIn, so the
	// host is the platform's own and only the last path element varies.
	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("probing the segment: %w", err)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusOK:
		return true, nil
	// A redirect means the object is routed rather than absent, which is
	// ordinary at a CDN edge. This client does not follow one, so it is read
	// here instead: treating it as a failure would leave every silenced hole
	// on that edge unanswered and unpatched.
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return true, nil
	case response.StatusCode == http.StatusForbidden, response.StatusCode == http.StatusNotFound:
		return false, nil
	// A method the edge will not answer is the edge's settled position, not a
	// bad moment, so it is an answer of no rather than a question to ask
	// again on every pass forever.
	case final(response.StatusCode):
		return false, nil
	default:
		return false, fmt.Errorf("the segment answered %d", response.StatusCode)
	}
}
