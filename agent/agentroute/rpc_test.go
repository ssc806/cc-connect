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

func (errWriteConn) ReadJSON(any) error  { return errors.New("errWriteConn: read not used") }
func (errWriteConn) WriteJSON(any) error { return errors.New("simulated write failure") }
func (errWriteConn) Close() error        { return nil }

func TestRPC_WriteFailureFailsClient(t *testing.T) {
	c := &rpcClient{
		conn:    errWriteConn{},
		opts:    options{requestTimeout: time.Second},
		pending: map[string]chan rpcResponse{},
		events:  make(chan sessionEventNotification, 1),
		closed:  make(chan struct{}),
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
