package agentroute

import (
	"context"
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
