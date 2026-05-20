package agentroute

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRPC_DialSendsRequiredHeaders(t *testing.T) {
	fs := newFakeServer(t)
	client, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	defer client.Close()

	fs.mu.Lock()
	auth, proto := fs.auth, fs.protocol
	fs.mu.Unlock()

	if auth != "Bearer "+testToken {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer "+testToken)
	}
	if proto != protocolHeader {
		t.Errorf("X-AgentRoute-Protocol header = %q, want %q", proto, protocolHeader)
	}
}

func TestRPC_RejectsUnsupportedProtocolHandshake(t *testing.T) {
	fs := newFakeServer(t)
	fs.rejectProtocol = true // server answers 426 Upgrade Required

	_, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err == nil {
		t.Fatal("expected an error when the server rejects the protocol handshake")
	}
	if !strings.Contains(err.Error(), "426") && !strings.Contains(strings.ToLower(err.Error()), "protocol") {
		t.Errorf("error should identify the 426/protocol rejection, got %q", err.Error())
	}
}

func TestRPC_RejectsUnauthorizedHandshake(t *testing.T) {
	fs := newFakeServer(t)
	fs.rejectAuth = true // server answers 401

	_, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err == nil {
		t.Fatal("expected an error when the server rejects auth")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(strings.ToLower(err.Error()), "auth") {
		t.Errorf("error should identify the 401 rejection, got %q", err.Error())
	}
}

func TestRPC_RejectsProtocolMismatchInHello(t *testing.T) {
	fs := newFakeServer(t)
	fs.helloProtocol = "some-other-protocol" // server negotiates a different protocol

	_, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err == nil {
		t.Fatal("expected an error when connection.hello negotiates a different protocol")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "protocol") {
		t.Errorf("error should identify the protocol mismatch, got %q", err.Error())
	}
}

func TestRPC_RejectsVersionMismatchInHello(t *testing.T) {
	fs := newFakeServer(t)
	fs.helloVersion = protocolVersion + 1 // server negotiates a future version

	_, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err == nil {
		t.Fatal("expected an error when connection.hello negotiates an incompatible version")
	}
}

func TestRPC_DialErrorDoesNotLeakToken(t *testing.T) {
	fs := newFakeServer(t)
	fs.rejectAuth = true

	_, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("dial error leaked the token: %q", err.Error())
	}
}

func TestRPC_CallReturnsTypedProtocolError(t *testing.T) {
	fs := newFakeServer(t)
	client, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	defer client.Close()

	// An unknown method makes the fake server reply with a JSON-RPC error.
	var out map[string]any
	err = client.call(context.Background(), "does.not.exist", map[string]any{}, &out)
	if err == nil {
		t.Fatal("expected an error for an unknown method")
	}
}

func TestRPC_NotificationsReachEventsChannel(t *testing.T) {
	fs := newFakeServer(t)
	fs.onStart = func(c *fakeConn, p sessionStartParams) {
		c.emitEvent("route_sess_fake", "run_1", "evt_1", 1, protocolEvent{Type: "text_delta", Text: "hi"})
	}
	client, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	defer client.Close()

	var res sessionStartResult
	if err := client.call(context.Background(), methodSessionStart, sessionStartParams{RequestID: "x"}, &res); err != nil {
		t.Fatalf("session.start: %v", err)
	}

	select {
	case note := <-client.events:
		if note.EventID != "evt_1" || note.RunID != "run_1" {
			t.Errorf("notification lost wrapper fields: %+v", note)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session.event notification never reached the events channel")
	}
}

// errWriteConn is a websocketConn whose writes always fail, used to exercise
// the write-failure path without a real connection.
type errWriteConn struct{}

func (errWriteConn) ReadJSON(any) error               { return errors.New("errWriteConn: read not used") }
func (errWriteConn) WriteJSON(any) error              { return errors.New("simulated write failure") }
func (errWriteConn) SetWriteDeadline(time.Time) error { return nil }
func (errWriteConn) Close() error                     { return nil }

// deadlineConn records the write deadline set before each WriteJSON call.
type deadlineConn struct {
	deadline time.Time
	written  bool
}

func (*deadlineConn) ReadJSON(any) error { return errors.New("deadlineConn: read not used") }
func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	c.deadline = t
	return nil
}
func (c *deadlineConn) WriteJSON(any) error { c.written = true; return nil }
func (*deadlineConn) Close() error          { return nil }

func TestRPC_WriteCarriesDeadline(t *testing.T) {
	dc := &deadlineConn{}
	c := &rpcClient{
		conn:     dc,
		opts:     options{requestTimeout: 30 * time.Second},
		pending:  map[string]chan rpcResponse{},
		events:   make(chan sessionEventNotification, 1),
		closed:   make(chan struct{}),
		writeSem: make(chan struct{}, 1),
	}

	// No read loop runs, so the call times out waiting for a response — but
	// the write must already have happened, bounded by a deadline.
	var res pingResult
	if err := c.callWithTimeout(context.Background(), 50*time.Millisecond, methodPing, pingParams{}, &res); err == nil {
		t.Fatal("expected a timeout error with no response")
	}
	if !dc.written {
		t.Fatal("WriteJSON was never called")
	}
	if dc.deadline.IsZero() {
		t.Error("write must be bounded by a deadline; SetWriteDeadline received the zero time")
	}
}

func TestRPC_WriteFailureFailsClient(t *testing.T) {
	c := &rpcClient{
		conn:     errWriteConn{},
		opts:     options{requestTimeout: time.Second},
		pending:  map[string]chan rpcResponse{},
		events:   make(chan sessionEventNotification, 1),
		closed:   make(chan struct{}),
		writeSem: make(chan struct{}, 1),
	}

	var res pingResult
	if err := c.call(context.Background(), methodPing, pingParams{}, &res); err == nil {
		t.Fatal("call must fail when the websocket write fails")
	}

	// A failed write must fail the client immediately so the engine recycles
	// the session instead of leaving a dead connection marked alive.
	select {
	case <-c.Done():
	default:
		t.Fatal("a failed write must fail the client so Done() is closed")
	}
}

// blockingWriteConn blocks inside WriteJSON until release is closed, so a test
// can pin the write slot and verify a later bounded write is not stalled past
// its own timeout. entered signals that WriteJSON has been reached.
type blockingWriteConn struct {
	entered chan struct{}
	release chan struct{}
}

func (*blockingWriteConn) ReadJSON(any) error               { return errors.New("blockingWriteConn: read not used") }
func (*blockingWriteConn) SetWriteDeadline(time.Time) error { return nil }
func (bc *blockingWriteConn) WriteJSON(any) error {
	select {
	case bc.entered <- struct{}{}:
	default:
	}
	<-bc.release
	return nil
}
func (*blockingWriteConn) Close() error { return nil }

// TestRPC_BoundedCallNotStalledByStuckWriter covers the Close() path: a normal
// RPC write stuck inside WriteJSON holds the write slot, and a later call with
// a small timeout (mirroring closeRPCTimeout) must still return near its own
// budget instead of waiting for the stuck writer.
func TestRPC_BoundedCallNotStalledByStuckWriter(t *testing.T) {
	bc := &blockingWriteConn{entered: make(chan struct{}, 1), release: make(chan struct{})}
	c := &rpcClient{
		conn:     bc,
		opts:     options{requestTimeout: 30 * time.Second},
		pending:  map[string]chan rpcResponse{},
		events:   make(chan sessionEventNotification, 1),
		closed:   make(chan struct{}),
		writeSem: make(chan struct{}, 1),
	}

	// A normal RPC write grabs the slot and stalls inside WriteJSON.
	stuck := make(chan error, 1)
	go func() {
		var res pingResult
		stuck <- c.callWithTimeout(context.Background(), 30*time.Second, methodPing, pingParams{}, &res)
	}()
	select {
	case <-bc.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first writer never reached WriteJSON")
	}

	// The bounded call must not wait for the stuck writer beyond its timeout.
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		var res sessionCancelResult
		done <- c.callWithTimeout(context.Background(), 100*time.Millisecond, methodSessionCancel, sessionCancelParams{}, &res)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("bounded call should fail while the write slot is held by a stuck writer")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("bounded call waited %s, want it bounded near its 100ms timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bounded call was not bounded — it waited for the stuck writer to finish")
	}

	close(bc.release) // let the stuck writer unwind
	<-stuck
}

func TestRPC_CloseFailsInFlightCalls(t *testing.T) {
	fs := newFakeServer(t)
	fs.startDelay = 3 * time.Second // server stalls the response
	client, err := newRPCClient(context.Background(), testOptions(fs.dialURL()))
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var res sessionStartResult
		done <- client.call(context.Background(), methodSessionStart, sessionStartParams{RequestID: "x"}, &res)
	}()

	time.Sleep(100 * time.Millisecond) // let the request reach the server
	client.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-flight call should fail once the client is closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call did not return after Close (it waited for the stalled server)")
	}
}
