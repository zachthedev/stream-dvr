package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Harness
// ///////////////////////////////////////////////

// receiver is a webhook endpoint on loopback. Every test in this file
// serves its own; nothing here reaches a real network.
type receiver struct {
	mu       sync.Mutex
	bodies   []string
	types    []string
	headers  []http.Header
	status   int
	body     string
	requests int
}

// serve starts the receiver and returns its address.
func (r *receiver) serve(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		r.mu.Lock()
		r.requests++
		r.bodies = append(r.bodies, string(body))
		r.types = append(r.types, req.Header.Get("Content-Type"))
		r.headers = append(r.headers, req.Header.Clone())
		status, reply := r.status, r.body
		r.mu.Unlock()

		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// delivered returns every body the receiver was posted.
func (r *receiver) delivered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.bodies...)
}

// quiet is a logger whose output nothing reads.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ///////////////////////////////////////////////
// Delivery
// ///////////////////////////////////////////////

func TestWebhook_PostsTheEventAsJSON(t *testing.T) {
	endpoint := &receiver{}
	hook := NewWebhook(endpoint.serve(t), quiet())

	at := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)
	if err := hook.Notify(context.Background(), Event{
		Kind:    "library_full",
		Channel: "twitch/examplechannel",
		Title:   "a broadcast",
		Detail:  "the cap refused it",
		At:      at,
	}); err != nil {
		t.Fatalf("Notify() err = %v, want nil", err)
	}
	// Close drains, so nothing here waits on a timer.
	hook.Close()

	delivered := endpoint.delivered()
	if len(delivered) != 1 {
		t.Fatalf("the receiver was posted %d times, want 1", len(delivered))
	}

	var got Event
	if err := json.Unmarshal([]byte(delivered[0]), &got); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if got.Kind != "library_full" || got.Channel != "twitch/examplechannel" {
		t.Errorf("the event arrived as %+v, want the one sent", got)
	}
	if !got.At.Equal(at) {
		t.Errorf("At = %s, want %s", got.At, at)
	}
	if endpoint.types[0] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", endpoint.types[0])
	}
}

func TestWebhook_StampsAnEventWithNoTime(t *testing.T) {
	// An event without a time reads as the zero date once it is JSON, which
	// puts a 1970s alert in a phone's notification list.
	endpoint := &receiver{}
	hook := NewWebhook(endpoint.serve(t), quiet())
	hook.now = func() time.Time { return time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC) }

	if err := hook.Notify(context.Background(), Event{Kind: "failure"}); err != nil {
		t.Fatalf("Notify() err = %v, want nil", err)
	}
	hook.Close()

	var got Event
	if err := json.Unmarshal([]byte(endpoint.delivered()[0]), &got); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if got.At.IsZero() {
		t.Error("At is the zero time, want the event stamped when it was queued")
	}
}

func TestWebhook_PutsNoEventTextInAHeader(t *testing.T) {
	// A header is a place nothing escapes text, and a title is written by
	// whoever was streaming.
	endpoint := &receiver{}
	hook := NewWebhook(endpoint.serve(t), quiet())

	if err := hook.Notify(context.Background(), Event{
		Kind:  "failure",
		Title: "a-very-distinctive-title",
	}); err != nil {
		t.Fatalf("Notify() err = %v, want nil", err)
	}
	hook.Close()

	for name, values := range endpoint.headers[0] {
		for _, value := range values {
			if strings.Contains(value, "a-very-distinctive-title") {
				t.Errorf("header %s carries the event title: %q", name, value)
			}
		}
	}
}

// ///////////////////////////////////////////////
// Never blocking the recording path
// ///////////////////////////////////////////////

func TestWebhook_NotifyDoesNotWaitForTheReceiver(t *testing.T) {
	// This runs on the recording path. Ten seconds spent on a webhook is
	// ten seconds of a broadcast not being recorded.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()

	// The hook is deliberately not closed. A regression leaves the sender
	// blocked inside Notify, and closing the queue under it would panic
	// over the assertion that reports what actually went wrong.
	hook := NewWebhook(server.URL, quiet())
	defer close(release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range queueDepth + 4 {
			if err := hook.Notify(context.Background(), Event{Kind: "failure"}); err != nil {
				t.Errorf("Notify() err = %v, want nil", err)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked on a receiver that is not answering")
	}
}

func TestWebhook_DropsRatherThanGrowingWithoutBound(t *testing.T) {
	// An unbounded queue turns an unreachable receiver into a memory leak
	// that outlives the outage, and a backlog of stale alerts is worth less
	// than the newest one.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()

	hook := NewWebhook(server.URL, quiet())
	defer func() { close(release); hook.Close() }()

	for range queueDepth * 4 {
		if err := hook.Notify(context.Background(), Event{Kind: "failure"}); err != nil {
			t.Fatalf("Notify() err = %v, want nil", err)
		}
	}

	if got := len(hook.queue); got > queueDepth {
		t.Errorf("the queue holds %d events, want at most %d", got, queueDepth)
	}
}

// ///////////////////////////////////////////////
// Failure
// ///////////////////////////////////////////////

func TestWebhook_ReportsWhatTheReceiverAnswered(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{name: "a refusal", status: http.StatusForbidden, want: "403"},
		{name: "a receiver that broke", status: http.StatusInternalServerError, want: "500"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := &receiver{status: tc.status}
			hook := NewWebhook(endpoint.serve(t), quiet())
			defer hook.Close()

			err := hook.send(Event{Kind: "failure"})

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("send() err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

func TestWebhook_NeverFollowsARedirect(t *testing.T) {
	// A receiver that answers with a redirect would otherwise walk the
	// request, and the secret in its path, to a host the operator never
	// named.
	elsewhere := &receiver{}
	elsewhereURL := elsewhere.serve(t)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, elsewhereURL, http.StatusFound)
	}))
	defer redirector.Close()

	hook := NewWebhook(redirector.URL, quiet())
	defer hook.Close()

	err := hook.send(Event{Kind: "failure"})

	if got := len(elsewhere.delivered()); got != 0 {
		t.Errorf("the redirect target was posted %d times, want none", got)
	}
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("send() err = %v, want the redirect reported as the answer", err)
	}
}

func TestWebhook_NeverNamesTheAddressInAFailure(t *testing.T) {
	// The path is often the only thing authorizing a post, so it is a
	// secret and the log is not the place for it.
	endpoint := &receiver{status: http.StatusForbidden}
	address := endpoint.serve(t) + "/hooks/a-secret-token"
	hook := NewWebhook(address, quiet())
	defer hook.Close()

	err := hook.send(Event{Kind: "failure"})

	if err == nil {
		t.Fatal("send() err = nil, want the refusal reported")
	}
	if strings.Contains(err.Error(), "a-secret-token") {
		t.Errorf("send() err = %v, want it to keep the address out", err)
	}
}

func TestWebhook_SurvivesAReceiverThatWillNotStopTalking(t *testing.T) {
	// A reply is read only so the connection can be reused. An unbounded
	// read would let a receiver spend this process's memory.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for range 200 {
			io.WriteString(w, strings.Repeat("x", 1024))
		}
	}))
	defer server.Close()

	hook := NewWebhook(server.URL, quiet())
	defer hook.Close()

	if err := hook.send(Event{Kind: "failure"}); err != nil {
		t.Errorf("send() err = %v, want the oversized reply tolerated", err)
	}
}

func TestWebhook_CloseIsIdempotent(t *testing.T) {
	// runServe defers it and the in-process recorder defers it too, so it
	// can be reached twice on one shutdown.
	endpoint := &receiver{}
	hook := NewWebhook(endpoint.serve(t), quiet())

	for range 3 {
		if err := hook.Close(); err != nil {
			t.Errorf("Close() err = %v, want nil", err)
		}
	}
}

func TestWebhook_CloseDeliversWhatIsQueued(t *testing.T) {
	// A daemon shutting down after a failure still has to deliver the
	// notification about it.
	endpoint := &receiver{}
	hook := NewWebhook(endpoint.serve(t), quiet())

	for range 5 {
		if err := hook.Notify(context.Background(), Event{Kind: "failure"}); err != nil {
			t.Fatalf("Notify() err = %v, want nil", err)
		}
	}
	hook.Close()

	if got := len(endpoint.delivered()); got != 5 {
		t.Errorf("the receiver was posted %d times after Close, want 5", got)
	}
}

func TestWebhook_NotifyAfterCloseDropsRatherThanPanicking(t *testing.T) {
	// Close closes the queue, and a send on a closed channel takes the
	// process down. The non-blocking select does not help: its default arm
	// is taken when the channel is full, never when it is closed. A sink
	// documented as never failing must not be the thing that kills a
	// capture in flight.
	hook := NewWebhook("https://example.invalid/hook", slog.New(slog.DiscardHandler))
	if err := hook.Close(); err != nil {
		t.Fatalf("Close() err = %v, want nil", err)
	}

	if err := hook.Notify(context.Background(), Event{Kind: "test"}); err != nil {
		t.Errorf("Notify() err = %v after Close, want it dropped quietly", err)
	}
}

func TestWebhook_CloseGivesUpOnAReceiverThatNeverAnswers(t *testing.T) {
	// A receiver that accepts the connection and then says nothing would
	// otherwise hold the shutdown for the queue depth times the send
	// timeout. A stop that reads as a hang is worse than a lost alert.
	hold := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-hold
	}))
	// Registered after the server, so it runs before it: closing the
	// server first would wait on the very handler this is holding.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(hold) })

	hook := NewWebhook(server.URL, slog.New(slog.DiscardHandler))
	for range queueDepth + 1 {
		if err := hook.Notify(context.Background(), Event{Kind: "test"}); err != nil {
			t.Fatalf("Notify() err = %v, want nil", err)
		}
	}

	started := time.Now()
	if err := hook.Close(); err != nil {
		t.Fatalf("Close() err = %v, want nil", err)
	}
	if held := time.Since(started); held > closeGrace+sendTimeout {
		t.Errorf("Close() held for %v, want it to give up near %v", held, closeGrace)
	}
}
