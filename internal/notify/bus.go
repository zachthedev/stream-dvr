package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Bus publishes events to whatever is listening on a local socket.
//
// It exists for one platform. Windows runs the recorder as a scheduled task
// in session 0, which has no desktop to post to and cannot start a process
// in the session that does. The helper has to be started by the session
// instead, and this is how it learns what happened.
//
// The bus never dials and never waits. Publishing is a non-blocking send
// into each subscriber's queue, so no subscriber, a slow subscriber, and a
// subscriber that stopped reading all cost the recording path nothing.
//
// # Trust
//
// The socket is authenticated by filesystem permissions and by nothing
// else. Any process running as this operator can connect and read every
// event, including channel names and broadcast titles. That is the same
// boundary as the config file and the database, both of which sit in the
// same profile, so the bus does not widen it. It does not narrow it either:
// this is not an authenticated channel and must not be described as one.
type Bus struct {
	listener net.Listener
	logger   *slog.Logger

	mu          sync.Mutex
	subscribers map[int]chan Event
	nextID      int
	closed      bool

	wg sync.WaitGroup
}

// handshake is the first line of every connection.
//
// A subscriber that does not know the version stops rather than guessing at
// the fields, which is the groundwork a format expected to gain events
// needs before it gains them.
type handshake struct {
	Protocol int `json:"protocol"`
}

// ///////////////////////////////////////////////
// Limits
// ///////////////////////////////////////////////

// ProtocolVersion is the wire format this build speaks.
const ProtocolVersion = 1

// maxSubscribers bounds how many connections the bus feeds at once.
//
// Fast user switching and a remote desktop session can each leave an agent
// running, and a TUI subscribes as well, so the ordinary count is two or
// three. A cap well above that still refuses a process opening connections
// in a loop.
const maxSubscribers = 8

// busWriteTimeout bounds one write to one subscriber.
//
// A subscriber that has stopped reading fills its queue and then blocks the
// socket. The deadline turns that into a closed connection instead of a
// goroutine held for as long as the process runs.
const busWriteTimeout = 2 * time.Second

// dialProbe bounds the one question asked before a socket left at the path
// is removed: is anything answering on it.
//
// A local socket answers immediately or not at all, so this is a guard
// against a wedged listener rather than a wait anything reaches.
const dialProbe = 250 * time.Millisecond

// maxEventLine bounds one line read from a bus.
//
// Every field is clipped long before this, so a line approaching it is not
// this project's writer. Ending the connection is the right answer to a
// peer that is not speaking the protocol.
const maxEventLine = 64 << 10

// minReconnect and maxReconnect bound how eagerly a subscriber redials.
//
// The recorder restarting is the common case and is over in under a
// second, so the first retry is quick. Nobody signed in for a working day
// is also common, and polling a path that is not there costs nothing worth
// spending, so the interval grows out to a minute.
const (
	minReconnect = time.Second
	maxReconnect = time.Minute
)

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

// Listen starts a bus on a socket path.
//
// A socket left by a previous run is removed first. Binding over one fails
// with "address already in use" on every platform, and a recorder that
// stopped without unlinking would otherwise never publish again.
//
// Whether it is stale is settled by asking it, not by assuming. This runs
// before the library is claimed, so the session row proves nothing here,
// and on Windows the filesystem does not either: a live listener's socket
// file can be removed out from under it. A socket that answers a dial has
// a listener behind it and is left alone.
func Listen(socketPath string, logger *slog.Logger) (*Bus, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Checked here rather than left to bind, whose refusal is "invalid
	// argument" and names neither the path nor the limit.
	if len(socketPath) > paths.MaxSocketPath {
		return nil, fmt.Errorf("the socket path %s is %d bytes, over the %d a socket address holds",
			socketPath, len(socketPath), paths.MaxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating the runtime directory: %w", err)
	}
	// Asked before it is removed. The claim is taken after this runs, so
	// nothing here holds the library yet and the reasoning above does not
	// apply at the moment it is relied on: a second recorder started by
	// hand would otherwise unlink a running one's socket, be refused the
	// library, and unlink the path again on its way out, leaving the
	// recorder that does hold the library listening where nothing can
	// reach it. A socket that answers belongs to a live listener.
	if answered := dialable(socketPath); answered {
		return nil, fmt.Errorf("a recorder is already listening on %s", socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing a socket left behind at %s: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	bus := &Bus{
		listener:    listener,
		logger:      logger,
		subscribers: make(map[int]chan Event),
	}

	bus.wg.Add(1)
	go bus.accept()
	return bus, nil
}

// dialable reports whether something is listening on a socket path.
//
// A stale socket left by a recorder that died refuses the connection, so
// only a live listener answers. The connection is closed at once; this
// asks a question and sends nothing.
func dialable(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, dialProbe)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Address returns the socket the bus is listening on.
func (b *Bus) Address() string { return b.listener.Addr().String() }

// Notify implements a sink. It never blocks and never fails.
//
// An event nobody is subscribed to is not an error: the agent may not have
// started yet, and the operator may not be signed in at all. The log and
// the webhook carry the event either way.
func (b *Bus) Notify(_ context.Context, event Event) error {
	// The lock is held across the sends. Each one is non-blocking, so the
	// critical section is bounded, and holding it is what keeps Close from
	// closing a queue this is sending into.
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, queue := range b.subscribers {
		select {
		case queue <- event:
		default:
			// Dropped for this subscriber alone. A notification is worth
			// less the later it arrives, and the alternative is holding the
			// recording path behind whoever is slowest.
			b.logger.Warn("dropped a notification because a subscriber is not keeping up",
				slog.String("kind", event.Kind))
		}
	}
	return nil
}

// Close stops listening and disconnects every subscriber.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	// Deleted as well as closed, which is what makes a closed bus one with
	// no subscribers. Publishing after this walks an empty map, so nothing
	// downstream needs its own check for a bus that has stopped.
	for id, queue := range b.subscribers {
		close(queue)
		delete(b.subscribers, id)
	}
	b.mu.Unlock()

	// Closing the listener ends the accept loop and unlinks the socket.
	err := b.listener.Close()
	b.wg.Wait()
	if err != nil {
		return fmt.Errorf("closing the notification socket: %w", err)
	}
	return nil
}

// ///////////////////////////////////////////////
// Serving
// ///////////////////////////////////////////////

// accept takes connections until the listener closes.
func (b *Bus) accept() {
	defer b.wg.Done()

	for {
		conn, err := b.listener.Accept()
		if err != nil {
			// The only way out. Close is the ordinary cause, and a listener
			// that failed for another reason cannot be recovered by looping
			// on it.
			return
		}

		queue, ok := b.register()
		if !ok {
			// At the cap. Refusing here rather than queueing means a process
			// opening connections in a loop cannot spend this one's memory.
			b.logger.Warn("refused a notification subscriber past the limit",
				slog.Int("limit", maxSubscribers))
			conn.Close()
			continue
		}

		b.wg.Add(1)
		go b.drain(conn, queue)
	}
}

// register adds a subscriber, reporting false when the bus is full or
// closed.
func (b *Bus) register() (chan Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed || len(b.subscribers) >= maxSubscribers {
		return nil, false
	}

	// Depth matches the webhook's, for the same reason: a backlog of stale
	// alerts is worth less than the newest one.
	queue := make(chan Event, queueDepth)
	b.subscribers[b.nextID] = queue
	b.nextID++
	return queue, true
}

// release removes a subscriber's queue so nothing publishes into it again.
func (b *Bus) release(queue chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, registered := range b.subscribers {
		if registered == queue {
			delete(b.subscribers, id)
			return
		}
	}
}

// drain writes one subscriber's events until it stops reading or the bus
// closes.
func (b *Bus) drain(conn net.Conn, queue chan Event) {
	defer b.wg.Done()
	defer conn.Close()
	defer b.release(queue)

	encoder := json.NewEncoder(conn)

	write := func(value any) error {
		if err := conn.SetWriteDeadline(time.Now().Add(busWriteTimeout)); err != nil {
			return err
		}
		return encoder.Encode(value)
	}

	if err := write(handshake{Protocol: ProtocolVersion}); err != nil {
		return
	}
	for event := range queue {
		if err := write(event); err != nil {
			// A subscriber that went away is the ordinary end of a
			// connection, not a fault worth a log line on every logout.
			b.logger.Debug("a notification subscriber stopped reading",
				slog.String("error", escape.Field(err.Error())))
			return
		}
	}
}

// ///////////////////////////////////////////////
// Subscribing
// ///////////////////////////////////////////////

// Follow delivers a bus's events to handle until ctx is done.
//
// It reconnects. The agent starts when the operator signs in, which is
// routinely before the recorder is running and always before the recorder
// next restarts, so a socket that is not there yet is the ordinary state
// rather than a failure. Only ctx ends this.
//
// handle runs on this goroutine, so a handler that blocks stops the
// subscriber reading, which is what the bus's write deadline is for.
func Follow(ctx context.Context, socketPath string, logger *slog.Logger, handle func(Event)) error {
	if logger == nil {
		logger = slog.Default()
	}

	backoff := minReconnect
	for {
		if err := readFrom(ctx, socketPath, handle); err != nil {
			logger.Debug("not following the notification socket",
				slog.String("error", escape.Field(err.Error())))
		} else {
			// A clean end means the connection stood up, so the next
			// attempt starts eager again.
			backoff = minReconnect
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxReconnect)
	}
}

// readFrom delivers one connection's events, returning when it ends.
func readFrom(ctx context.Context, socketPath string, handle func(Event)) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Nothing else unblocks a read on a socket the recorder has gone quiet
	// on, and the agent has to stop when the session ends.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	scanner := bufio.NewScanner(conn)
	// Starts small and grows to the bound. An ordinary event is a few
	// hundred bytes, and a subscriber holds this for as long as it runs.
	scanner.Buffer(make([]byte, 0, 4<<10), maxEventLine)

	if !scanner.Scan() {
		return fmt.Errorf("reading the protocol line: %w", scanner.Err())
	}
	var greeting handshake
	if err := json.Unmarshal(scanner.Bytes(), &greeting); err != nil {
		return fmt.Errorf("parsing the protocol line: %w", err)
	}
	if greeting.Protocol != ProtocolVersion {
		return fmt.Errorf("the recorder speaks protocol %d and this build speaks %d",
			greeting.Protocol, ProtocolVersion)
	}

	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			// One unreadable line is not a reason to drop the connection
			// carrying the readable ones after it.
			continue
		}
		handle(event)
	}
	return scanner.Err()
}
