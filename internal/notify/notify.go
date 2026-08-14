// Package notify delivers stream-dvr's events somewhere the operator will
// see them.
//
// The recorder's normal mode is a scheduled task or a systemd unit running
// with nobody signed in, which is the constraint every sink here has to
// meet. An HTTP post meets it without help: it reaches a phone, a
// self-hosted receiver, or a chat room from a process with no session, and
// costs no third-party dependency.
//
// Nothing here blocks the recording path. A send is queued and a single
// goroutine drains the queue, so a receiver that has gone away delays no
// capture.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Event is one notification.
//
// It mirrors the daemon's event rather than importing it, the way
// space.Limits and retention.Policy mirror the config: a sink is testable
// without the daemon, and a rename there is not a change to what a
// notification means.
type Event struct {
	// Kind classifies the event, such as "library_full".
	Kind string `json:"kind"`
	// Channel is the channel involved, when there is one.
	Channel string `json:"channel,omitempty"`
	// Title is the broadcast title, when known.
	Title string `json:"title,omitempty"`
	// Detail explains what happened.
	Detail string `json:"detail,omitempty"`
	// At is when it happened, in RFC 3339.
	At time.Time `json:"at"`
}

// Webhook posts events to an HTTP endpoint.
type Webhook struct {
	address string
	client  *http.Client
	logger  *slog.Logger
	now     func() time.Time

	queue chan Event
	done  chan struct{}
	once  sync.Once

	// mu guards closed against queue. A send on a closed channel panics,
	// and the non-blocking select does not help: the default arm is taken
	// when the channel is full, never when it is closed.
	mu     sync.Mutex
	closed bool
	// stop cancels a post in flight, so a shutdown is not held by a
	// receiver that accepted the connection and then said nothing.
	stop context.CancelFunc
	// sending carries the context every post is built against.
	sending context.Context
}

// ///////////////////////////////////////////////
// Limits
// ///////////////////////////////////////////////

// sendTimeout bounds one post. It is generous for a receiver that answers
// and short enough that a queue behind a dead one drains rather than stalls.
const sendTimeout = 10 * time.Second

// closeGrace bounds how long a shutdown waits for the queue to drain.
//
// Close waits so a daemon stopping after a failure still delivers the
// notification about it. A receiver that accepts a connection and never
// answers turns that into the queue depth times the send timeout, minutes
// of a process that is already finished. Losing a queued alert is a better
// outcome than a stop that reads as a hang, and on Windows the service
// manager's own patience is shorter than the wait it would replace.
const closeGrace = 5 * time.Second

// queueDepth is how many events wait for a slow receiver.
//
// Small on purpose. A backlog of stale alerts is worth less than the newest
// one, and an unbounded queue turns an unreachable receiver into a memory
// leak that outlives the outage.
const queueDepth = 16

// maxResponse bounds what a reply may spend. Nothing here reads the body;
// it is drained only so the connection can be reused.
const maxResponse = 4 << 10

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

// NewWebhook starts a sink posting to address.
//
// The address is the operator's validated config value. Validation refuses
// a scheme other than http or https, a credentialed URL, and a link-local
// host, so this does not repeat those checks.
//
// Redirects are refused rather than followed. A receiver that answers with
// a redirect would otherwise walk the request, and its secret-bearing path,
// to a host the operator never named.
func NewWebhook(address string, logger *slog.Logger) *Webhook {
	if logger == nil {
		logger = slog.Default()
	}

	hook := &Webhook{
		address: address,
		logger:  logger,
		now:     time.Now,
		client: &http.Client{
			Timeout: sendTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		queue: make(chan Event, queueDepth),
		done:  make(chan struct{}),
	}
	hook.sending, hook.stop = context.WithCancel(context.Background())

	go hook.run()
	return hook
}

// Notify queues an event and returns at once.
//
// It never blocks and never fails. This runs on the recording path, where
// ten seconds spent on a webhook is ten seconds of a broadcast not being
// recorded, and no alert is worth that.
func (w *Webhook) Notify(_ context.Context, event Event) error {
	if event.At.IsZero() {
		event.At = w.now()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// After Close the queue is closed, and sending into it would take
		// the process down rather than drop one notification.
		w.logger.Warn("dropped a notification because the webhook is closed",
			slog.String("kind", event.Kind))
		return nil
	}

	select {
	case w.queue <- event:
	default:
		// Dropped rather than queued behind a receiver that is not
		// answering. The log is the sink that never fails.
		w.logger.Warn("dropped a notification because the webhook queue is full",
			slog.String("kind", event.Kind))
	}
	return nil
}

// Close drains what is queued and stops the sender.
//
// It waits, so a daemon shutting down after a failure still delivers the
// notification about it.
func (w *Webhook) Close() error {
	w.once.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.queue)
		w.mu.Unlock()

		select {
		case <-w.done:
		case <-time.After(closeGrace):
			// The sender is wedged on a receiver that is not answering.
			// Cancelling the post in flight lets the drain finish rather
			// than holding a stop for the rest of the queue's timeouts.
			w.stop()
			<-w.done
		}
	})
	return nil
}

// run posts queued events one at a time until the queue closes.
func (w *Webhook) run() {
	defer close(w.done)

	for event := range w.queue {
		if err := w.send(event); err != nil {
			// The address is not logged. Its path is often the only thing
			// authorizing a post to it.
			w.logger.Warn("could not deliver a notification",
				slog.String("kind", event.Kind),
				slog.String("error", escape.Field(err.Error())))
		}
	}
}

// send posts one event.
func (w *Webhook) send(event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encoding the event: %w", err)
	}

	// The scheme is checked here as well as at load, because this is the
	// line that turns a config value into a request, and a check anywhere
	// else is a check something can be routed around.
	address, err := url.Parse(w.address)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") {
		return errors.New("the address is not an http or https URL")
	}

	// The client's timeout covers the whole exchange. The context adds the
	// one thing it cannot: a shutdown that has waited long enough cancels
	// the post rather than waiting out its timeout.
	request, err := http.NewRequestWithContext(
		w.sending, http.MethodPost, address.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	// The only header carrying anything is the content type. Event text in a
	// header is text in a place nothing escapes it.
	request.Header.Set("Content-Type", "application/json")

	// The address is an operator-configured webhook endpoint, which is the
	// whole feature. It is validated at load for scheme, credentials and a
	// link-local host, and for scheme again immediately above. gosec cannot
	// see either check.
	// G704: the destination is validated config, not user input
	response, err := w.client.Do(request)
	if err != nil {
		return errors.New("the receiver did not answer")
	}
	defer response.Body.Close()

	// Read and discard so the connection is reusable, bounded so a receiver
	// answering with a stream cannot spend this process's memory. A short
	// read is the ordinary case and changes nothing here.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("the receiver answered %d", response.StatusCode)
	}
	return nil
}
