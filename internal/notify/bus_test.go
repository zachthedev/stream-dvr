package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Test helpers
// ///////////////////////////////////////////////

// subscriber is a test client holding the connection its scanner reads.
type subscriber struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

// busWait bounds every read a test waits on.
//
// It is a liveness guard, not a measurement. A bus that publishes answers
// in microseconds and a bus that never publishes are what these tests tell
// apart, and no amount of waiting turns the second into the first, so the
// bound is set where a loaded machine cannot reach it.
const busWait = 30 * time.Second

// socketPath returns a short socket path that the test cleans up.
//
// Deliberately not t.TempDir: it embeds the test's name, and the names here
// are long enough that the resulting path overruns the 107-byte address
// field, which fails as "invalid argument" rather than as anything that
// names the cause.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatalf("creating a socket directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "n.sock")
	if len(path) > paths.MaxSocketPath {
		t.Fatalf("socket path %q is %d bytes, over the %d-byte limit", path, len(path), paths.MaxSocketPath)
	}
	return path
}

// startBus returns a listening bus that closes with the test.
func startBus(t *testing.T) *Bus {
	t.Helper()

	bus, err := Listen(socketPath(t), quiet())
	if err != nil {
		t.Fatalf("starting a bus: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

// subscribe connects to a bus and consumes the protocol line, so that a
// returned subscriber is one the bus has registered.
func subscribe(t *testing.T, bus *Bus) *subscriber {
	t.Helper()

	conn, err := net.Dial("unix", bus.Address())
	if err != nil {
		t.Fatalf("dialing the bus: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	sub := &subscriber{conn: conn, scanner: bufio.NewScanner(conn)}
	if got := sub.protocol(t); got != ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", got, ProtocolVersion)
	}
	return sub
}

// protocol reads the handshake.
func (s *subscriber) protocol(t *testing.T) int {
	t.Helper()

	var greeting handshake
	if err := json.Unmarshal(s.line(t), &greeting); err != nil {
		t.Fatalf("parsing the protocol line: %v", err)
	}
	return greeting.Protocol
}

// next reads one event.
func (s *subscriber) next(t *testing.T) Event {
	t.Helper()

	var event Event
	if err := json.Unmarshal(s.line(t), &event); err != nil {
		t.Fatalf("parsing an event: %v", err)
	}
	return event
}

// line reads one line, failing the test rather than hanging.
func (s *subscriber) line(t *testing.T) []byte {
	t.Helper()

	if err := s.conn.SetReadDeadline(time.Now().Add(busWait)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	if !s.scanner.Scan() {
		t.Fatalf("expected a line, got %v", s.scanner.Err())
	}
	return s.scanner.Bytes()
}

// ///////////////////////////////////////////////
// Listen
// ///////////////////////////////////////////////

func TestListen_RemovesASocketLeftBehind(t *testing.T) {
	// A recorder killed rather than stopped leaves the file, and binding
	// over one fails. Without this, one hard shutdown ends notifications
	// until somebody deletes a file they have no reason to know about.
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("left behind"), 0o600); err != nil {
		t.Fatalf("writing a stale socket: %v", err)
	}

	bus, err := Listen(path, quiet())
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
}

func TestListen_CreatesTheRuntimeDirectory(t *testing.T) {
	// The runtime directory is not one anything else creates on Windows or
	// macOS, so the first daemon to start has to.
	dir, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatalf("creating a parent directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "sub", "n.sock")
	bus, err := Listen(path, quiet())
	if err != nil {
		t.Fatalf("Listen under a missing directory: %v", err)
	}
	t.Cleanup(func() { bus.Close() })
}

func TestListen_ReportsAPathItCannotBind(t *testing.T) {
	// A path over the address limit fails at bind. The daemon has to say so
	// at startup rather than discover it at the first event.
	dir, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatalf("creating a parent directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	long := filepath.Join(dir, strings.Repeat("x", paths.MaxSocketPath), "n.sock")
	bus, err := Listen(long, quiet())
	if err == nil {
		bus.Close()
		t.Fatal("Listen on an unbindable path succeeded, want an error")
	}
}

// ///////////////////////////////////////////////
// Publishing
// ///////////////////////////////////////////////

func TestBus_GreetsWithTheProtocolVersion(t *testing.T) {
	// The first line is the version, so a subscriber built against another
	// one stops instead of reading fields it does not know.
	subscribe(t, startBus(t))
}

func TestBus_DeliversAnEvent(t *testing.T) {
	bus := startBus(t)
	sub := subscribe(t, bus)

	want := Event{Kind: "recording_started", Channel: "examplechannel", Title: "a broadcast"}
	if err := bus.Notify(context.Background(), want); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got := sub.next(t)
	if got.Kind != want.Kind || got.Channel != want.Channel || got.Title != want.Title {
		t.Errorf("received %+v, want %+v", got, want)
	}
}

func TestBus_DeliversToEverySubscriber(t *testing.T) {
	// Fast user switching leaves two agents alive and the TUI subscribes as
	// well, so one event reaching one of them is not enough.
	bus := startBus(t)
	first := subscribe(t, bus)
	second := subscribe(t, bus)

	if err := bus.Notify(context.Background(), Event{Kind: "failure"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if got := first.next(t).Kind; got != "failure" {
		t.Errorf("first subscriber received %q, want %q", got, "failure")
	}
	if got := second.next(t).Kind; got != "failure" {
		t.Errorf("second subscriber received %q, want %q", got, "failure")
	}
}

func TestBus_PreservesEveryField(t *testing.T) {
	// The agent renders from these, so a field dropped on the wire is a
	// notification that says less than the log does.
	bus := startBus(t)
	sub := subscribe(t, bus)

	want := Event{
		Kind:    "library_full",
		Channel: "examplechannel",
		Title:   "a title with \"quotes\" and a \n newline",
		Detail:  "no room left",
		At:      time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC),
	}
	if err := bus.Notify(context.Background(), want); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got := sub.next(t)
	if got.Kind != want.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, want.Kind)
	}
	if got.Channel != want.Channel {
		t.Errorf("Channel = %q, want %q", got.Channel, want.Channel)
	}
	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
	if got.Detail != want.Detail {
		t.Errorf("Detail = %q, want %q", got.Detail, want.Detail)
	}
	if !got.At.Equal(want.At) {
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
}

func TestBus_PublishesWithNobodyListening(t *testing.T) {
	// The ordinary state: nobody is signed in, so no agent is running. It
	// must not be an error and must not block the recording path.
	bus := startBus(t)

	if err := bus.Notify(context.Background(), Event{Kind: "downtime"}); err != nil {
		t.Errorf("Notify with no subscribers = %v, want nil", err)
	}
}

func TestBus_NeverBlocksOnAFullQueue(t *testing.T) {
	// The whole reason publishing is a non-blocking send. A capture must
	// cost nothing when a subscriber wedges.
	//
	// The queue is registered without a reader and filled here, rather than
	// by publishing at a connected subscriber that does not read: the
	// socket absorbs whatever it buffers first, so that route reaches the
	// full queue only by guessing at a buffer size, or never.
	bus := startBus(t)

	queue, ok := bus.register()
	if !ok {
		t.Fatal("register refused the first subscriber")
	}
	for range cap(queue) {
		queue <- Event{Kind: "filler"}
	}

	published := make(chan struct{})
	go func() {
		defer close(published)
		bus.Notify(context.Background(), Event{Kind: "recording_started"})
	}()

	select {
	case <-published:
	case <-time.After(busWait):
		t.Fatal("Notify blocked on a subscriber whose queue is full")
	}
}

func TestBus_RefusesSubscribersPastTheLimit(t *testing.T) {
	// A process opening connections in a loop must not spend this one's
	// memory. Each is subscribed in turn, so the cap is reached rather than
	// raced.
	bus := startBus(t)
	for range maxSubscribers {
		subscribe(t, bus)
	}

	conn, err := net.Dial("unix", bus.Address())
	if err != nil {
		// Refusing the connection outright is also a correct answer.
		return
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(busWait)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	if scanner := bufio.NewScanner(conn); scanner.Scan() {
		t.Errorf("a subscriber past the limit was greeted with %q, want no greeting", scanner.Text())
	}
}

func TestBus_StopsPublishingToASubscriberThatLeft(t *testing.T) {
	// A queue left registered behind a departed subscriber is a slot the
	// cap never gets back, so the ninth agent of a long uptime is refused
	// for nothing.
	bus := startBus(t)
	sub := subscribe(t, bus)
	sub.conn.Close()

	// The write that discovers the closed connection is what deregisters
	// it, so the count settles only once one has been attempted.
	deadline := time.Now().Add(busWait)
	for time.Now().Before(deadline) {
		bus.Notify(context.Background(), Event{Kind: "downtime"})

		bus.mu.Lock()
		remaining := len(bus.subscribers)
		bus.mu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("a subscriber that disconnected is still registered")
}

// ///////////////////////////////////////////////
// Close
// ///////////////////////////////////////////////

func TestBus_CloseDisconnectsSubscribers(t *testing.T) {
	bus := startBus(t)
	sub := subscribe(t, bus)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := sub.conn.SetReadDeadline(time.Now().Add(busWait)); err != nil {
		t.Fatalf("setting a read deadline: %v", err)
	}
	if scanner := bufio.NewScanner(sub.conn); scanner.Scan() {
		t.Errorf("read %q after Close, want the connection ended", scanner.Text())
	}
}

func TestBus_CloseIsRepeatable(t *testing.T) {
	// Both the daemon's shutdown path and a deferred close reach this, and
	// a second call must not panic on an already-closed channel.
	bus := startBus(t)
	if err := bus.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestBus_NotifyAfterCloseIsQuiet(t *testing.T) {
	// The daemon's sinks close before its last events are certain to have
	// stopped, and a send on a closed channel panics.
	bus := startBus(t)
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bus.Notify(context.Background(), Event{Kind: "failure"}); err != nil {
		t.Errorf("Notify after Close = %v, want nil", err)
	}
}

func TestBus_CloseUnlinksTheSocket(t *testing.T) {
	// A file left at the path is what the next Listen has to clear, and one
	// removed at close is one the operator never sees.
	path := socketPath(t)
	bus, err := Listen(path, quiet())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket survives Close: %v", err)
	}
}

// ///////////////////////////////////////////////
// Follow
// ///////////////////////////////////////////////

func TestFollow_DeliversEvents(t *testing.T) {
	bus := startBus(t)

	ctx := t.Context()

	received := make(chan Event, 1)
	go Follow(ctx, bus.Address(), quiet(), func(event Event) {
		received <- event
	})

	// The subscription is asynchronous, so the event is republished until
	// one lands rather than published once into a bus nobody has reached.
	go func() {
		for ctx.Err() == nil {
			bus.Notify(ctx, Event{Kind: "recording_started", Channel: "examplechannel"})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case event := <-received:
		if event.Channel != "examplechannel" {
			t.Errorf("Channel = %q, want %q", event.Channel, "examplechannel")
		}
	case <-time.After(busWait):
		t.Fatal("Follow delivered nothing")
	}
}

func TestFollow_StopsWhenTheContextIsDone(t *testing.T) {
	// The agent has to end when the session does, and it spends most of its
	// life blocked on a read that nothing else interrupts.
	bus := startBus(t)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- Follow(ctx, bus.Address(), quiet(), func(Event) {}) }()

	cancel()
	select {
	case err := <-stopped:
		if err == nil {
			t.Error("Follow returned nil, want the context's error")
		}
	case <-time.After(busWait):
		t.Fatal("Follow ignored a cancelled context")
	}
}

func TestFollow_WaitsForABusThatIsNotThereYet(t *testing.T) {
	// The agent starts when the operator signs in, routinely before the
	// recorder is running. Refusing at that point would mean no
	// notifications until the next logon.
	path := socketPath(t)

	ctx := t.Context()

	received := make(chan Event, 1)
	go Follow(ctx, path, quiet(), func(event Event) {
		received <- event
	})

	bus, err := Listen(path, quiet())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { bus.Close() })

	go func() {
		for ctx.Err() == nil {
			bus.Notify(ctx, Event{Kind: "downtime"})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case event := <-received:
		if event.Kind != "downtime" {
			t.Errorf("Kind = %q, want %q", event.Kind, "downtime")
		}
	case <-time.After(busWait):
		t.Fatal("Follow never reached a bus that started after it")
	}
}

func TestFollow_ReconnectsAfterTheRecorderRestarts(t *testing.T) {
	// The recorder restarts on an upgrade and on a reboot, and the agent
	// outlives both. One that gave up would go quiet until the next logon.
	path := socketPath(t)

	first, err := Listen(path, quiet())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx := t.Context()

	received := make(chan Event, 8)
	go Follow(ctx, path, quiet(), func(event Event) {
		select {
		case received <- event:
		default:
		}
	})

	// Reach the first bus, so what follows is a reconnection rather than a
	// first connection.
	go func() {
		for ctx.Err() == nil {
			first.Notify(ctx, Event{Kind: "before"})
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-received:
	case <-time.After(busWait):
		t.Fatal("Follow never reached the first bus")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("closing the first bus: %v", err)
	}

	second, err := Listen(path, quiet())
	if err != nil {
		t.Fatalf("restarting the bus: %v", err)
	}
	t.Cleanup(func() { second.Close() })

	go func() {
		for ctx.Err() == nil {
			second.Notify(ctx, Event{Kind: "after"})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	deadline := time.After(busWait)
	for {
		select {
		case event := <-received:
			if event.Kind == "after" {
				return
			}
		case <-deadline:
			t.Fatal("Follow never reconnected to the restarted bus")
		}
	}
}

// ///////////////////////////////////////////////
// readFrom
// ///////////////////////////////////////////////

// serveLines answers one connection with canned lines, so that a
// subscriber can be tested against a peer the bus would never produce.
func serveLines(t *testing.T, lines ...string) string {
	t.Helper()

	path := socketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for _, line := range lines {
			if _, err := fmt.Fprintln(conn, line); err != nil {
				return
			}
		}
	}()
	return path
}

func TestReadFrom_RefusesAnUnknownProtocol(t *testing.T) {
	// The reason the version is on the wire. A subscriber that read on
	// would be guessing at fields whose meaning changed.
	path := serveLines(t, `{"protocol":99}`, `{"kind":"recording_started"}`)

	var delivered []Event
	err := readFrom(context.Background(), path, func(event Event) {
		delivered = append(delivered, event)
	})

	if err == nil {
		t.Error("readFrom accepted protocol 99, want an error")
	}
	if len(delivered) != 0 {
		t.Errorf("delivered %d events past an unknown protocol, want 0", len(delivered))
	}
}

func TestReadFrom_RefusesAnUnparseableProtocolLine(t *testing.T) {
	path := serveLines(t, `not json`, `{"kind":"recording_started"}`)

	var delivered []Event
	err := readFrom(context.Background(), path, func(event Event) {
		delivered = append(delivered, event)
	})

	if err == nil {
		t.Error("readFrom accepted an unparseable protocol line, want an error")
	}
	if len(delivered) != 0 {
		t.Errorf("delivered %d events past an unparseable greeting, want 0", len(delivered))
	}
}

func TestReadFrom_SkipsAnUnreadableLine(t *testing.T) {
	// One bad line must not cost the readable ones after it, or a single
	// malformed event silences the agent until the recorder restarts.
	path := serveLines(t,
		fmt.Sprintf(`{"protocol":%d}`, ProtocolVersion),
		`{"kind":"first"}`,
		`{ not json`,
		`{"kind":"second"}`)

	var delivered []Event
	if err := readFrom(context.Background(), path, func(event Event) {
		delivered = append(delivered, event)
	}); err != nil {
		t.Fatalf("readFrom: %v", err)
	}

	var kinds []string
	for _, event := range delivered {
		kinds = append(kinds, event.Kind)
	}
	if want := []string{"first", "second"}; strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("delivered %v, want %v", kinds, want)
	}
}

func TestReadFrom_RefusesALinePastTheBound(t *testing.T) {
	// A peer sending an unbounded line is not this project's writer, and
	// reading it would let one spend the agent's memory.
	path := serveLines(t,
		fmt.Sprintf(`{"protocol":%d}`, ProtocolVersion),
		`{"kind":"first"}`,
		`{"kind":"`+strings.Repeat("x", maxEventLine)+`"}`,
		`{"kind":"never read"}`)

	var delivered []Event
	err := readFrom(context.Background(), path, func(event Event) {
		delivered = append(delivered, event)
	})

	if err == nil {
		t.Error("readFrom accepted a line past the bound, want an error")
	}
	for _, event := range delivered {
		if event.Kind == "never read" {
			t.Error("readFrom carried on past a line it could not hold")
		}
	}
}

func TestReadFrom_ReportsASocketThatIsNotThere(t *testing.T) {
	// Follow turns this into a retry, so it has to arrive as an error
	// rather than as a connection that delivers nothing.
	err := readFrom(context.Background(), socketPath(t), func(Event) {})
	if err == nil {
		t.Error("readFrom on a missing socket returned nil, want an error")
	}
}

func TestListen_RefusesToUnlinkASocketSomethingIsListeningOn(t *testing.T) {
	// Binding happens before the library is claimed, so a second recorder
	// started by hand reaches here holding nothing. Removing the path on
	// the way in and unlinking it again on the way out would leave the
	// recorder that does hold the library listening where its own agent
	// can never reach it, and nothing says so above Debug.
	socket := socketPath(t)

	running, err := Listen(socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Listen() err = %v, want the first bus to bind", err)
	}
	t.Cleanup(func() { _ = running.Close() })

	if _, err := Listen(socket, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Listen() bound over a socket with a live listener, want a refusal")
	}

	// The running bus still has its socket, and it still publishes.
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the running bus lost its socket: %v", err)
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatalf("the running bus is unreachable: %v", err)
	}
	_ = conn.Close()
}

func TestListen_RemovesASocketNothingIsListeningOn(t *testing.T) {
	// The stale case the removal exists for: a recorder that stopped
	// without unlinking must not lock the path for good.
	socket := socketPath(t)

	stale, err := Listen(socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Listen() err = %v, want the first bus to bind", err)
	}
	// Close the listener but leave the file, which is what an unclean stop
	// leaves on Windows.
	if err := stale.Close(); err != nil {
		t.Fatalf("Close() err = %v, want nil", err)
	}
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatalf("staging a stale socket file: %v", err)
	}

	next, err := Listen(socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Listen() err = %v, want a stale socket to be replaced", err)
	}
	_ = next.Close()
}
