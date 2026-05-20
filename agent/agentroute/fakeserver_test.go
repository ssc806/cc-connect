package agentroute

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testToken = "test-secret-token"

// fakeServer is an in-memory JSON-RPC WebSocket server used to exercise the
// agentroute adapter without a real agent-route service. It inspects the HTTP
// upgrade headers and rejects regressions, records every request's params by
// method, and lets tests script server-pushed session.event notifications.
type fakeServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	auth     string
	protocol string
	ccVer    string
	ccProj   string
	requests map[string][]json.RawMessage

	// knobs — set by the test before the adapter connects.
	rejectAuth       bool          // force HTTP 401
	rejectProtocol   bool          // force HTTP 426
	startDelay       time.Duration // delay before answering session.start
	rejectSend       bool          // answer session.send with accepted=false
	rejectPermission bool          // answer permission.respond with accepted=false
	stallCancelClose time.Duration // delay before answering session.cancel/close
	sessionID        string        // session_id returned by session.start
	listResult       sessionListResult
	onSend           func(c *fakeConn, p sessionSendParams)
	onStart          func(c *fakeConn, p sessionStartParams)

	connCh chan *fakeConn
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		requests: map[string][]json.RawMessage{},
		connCh:   make(chan *fakeConn, 4),
	}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	t.Cleanup(fs.srv.Close)
	return fs
}

// dialURL is the ws:// endpoint the adapter should connect to.
func (fs *fakeServer) dialURL() string {
	return "ws" + strings.TrimPrefix(fs.srv.URL, "http") + "/v1/agent-sessions"
}

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	fs.auth = r.Header.Get("Authorization")
	fs.protocol = r.Header.Get("X-AgentRoute-Protocol")
	fs.ccVer = r.Header.Get("X-CC-Connect-Version")
	fs.ccProj = r.Header.Get("X-CC-Connect-Project")
	rejectAuth, rejectProto := fs.rejectAuth, fs.rejectProtocol
	auth, proto := fs.auth, fs.protocol
	fs.mu.Unlock()

	// Reject handshake regressions so a missing header fails the test.
	if rejectAuth || !strings.HasPrefix(auth, "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if rejectProto || proto != protocolHeader {
		w.WriteHeader(http.StatusUpgradeRequired) // 426
		return
	}

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	fc := &fakeConn{conn: conn, fs: fs}
	select {
	case fs.connCh <- fc:
	default:
	}
	fc.serve()
}

func (fs *fakeServer) record(method string, params json.RawMessage) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	cp := make(json.RawMessage, len(params))
	copy(cp, params)
	fs.requests[method] = append(fs.requests[method], cp)
}

// count returns how many times a method was received.
func (fs *fakeServer) count(method string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.requests[method])
}

// lastParams decodes the most recent params for method into v.
func (fs *fakeServer) lastParams(t *testing.T, method string, v any) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	got := fs.requests[method]
	if len(got) == 0 {
		t.Fatalf("fake server never received %s", method)
	}
	if err := json.Unmarshal(got[len(got)-1], v); err != nil {
		t.Fatalf("decode %s params: %v", method, err)
	}
}

// allParams decodes every recorded params for method into a slice.
func (fs *fakeServer) allParams(t *testing.T, method string) []json.RawMessage {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]json.RawMessage, len(fs.requests[method]))
	copy(out, fs.requests[method])
	return out
}

// waitConn blocks until a client connection has been accepted.
func (fs *fakeServer) waitConn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-fs.connCh:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client connection")
		return nil
	}
}

// fakeConn is one accepted server-side connection.
type fakeConn struct {
	conn    *websocket.Conn
	fs      *fakeServer
	writeMu sync.Mutex
}

func (fc *fakeConn) serve() {
	defer fc.conn.Close()
	for {
		var in rpcIncoming
		if err := fc.conn.ReadJSON(&in); err != nil {
			return
		}
		fc.fs.record(in.Method, in.Params)
		fc.dispatch(in)
	}
}

func (fc *fakeConn) dispatch(in rpcIncoming) {
	fs := fc.fs
	switch in.Method {
	case methodConnectionHello:
		fc.reply(in.ID, helloResult{
			ConnectionID: "conn_fake",
			Protocol:     protocolName,
			Version:      protocolVersion,
			Capabilities: []string{"text", "stream", "permission", "cancel", "session_list", "resume"},
		})
	case methodSessionStart:
		fs.mu.Lock()
		delay, sid, onStart := fs.startDelay, fs.sessionID, fs.onStart
		fs.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		var p sessionStartParams
		_ = json.Unmarshal(in.Params, &p)
		if sid == "" {
			sid = "route_sess_fake"
		}
		if p.ResumeSessionID != "" {
			sid = p.ResumeSessionID
		}
		fc.reply(in.ID, sessionStartResult{
			SessionID:          sid,
			CCConnectSessionID: p.CCConnectSessionID,
			AgentType:          "codex",
			Status:             "ready",
			Recoverable:        true,
		})
		if onStart != nil {
			onStart(fc, p)
		}
	case methodSessionSend:
		var p sessionSendParams
		_ = json.Unmarshal(in.Params, &p)
		fs.mu.Lock()
		rejectSend, onSend := fs.rejectSend, fs.onSend
		fs.mu.Unlock()
		status := "running"
		if rejectSend {
			status = "rejected"
		}
		fc.reply(in.ID, sessionSendResult{
			SessionID: p.SessionID,
			RunID:     p.RunID, // echo the client-generated run_id
			Accepted:  !rejectSend,
			Status:    status,
		})
		if onSend != nil {
			onSend(fc, p)
		}
	case methodPermissionRespond:
		fs.mu.Lock()
		rejectPerm := fs.rejectPermission
		fs.mu.Unlock()
		fc.reply(in.ID, permissionRespondResult{Accepted: !rejectPerm})
	case methodSessionCancel:
		fs.mu.Lock()
		stall := fs.stallCancelClose
		fs.mu.Unlock()
		fc.replyMaybeStalled(in.ID, stall, sessionCancelResult{Accepted: true, Status: "cancelling"})
	case methodSessionClose:
		fs.mu.Lock()
		stall := fs.stallCancelClose
		fs.mu.Unlock()
		fc.replyMaybeStalled(in.ID, stall, sessionCloseResult{Closed: true})
	case methodSessionList:
		fs.mu.Lock()
		res := fs.listResult
		fs.mu.Unlock()
		fc.reply(in.ID, res)
	case methodPing:
		now := time.Now().UTC().Format(time.RFC3339)
		fc.reply(in.ID, pingResult{TS: now, ServerTime: now})
	default:
		fc.replyError(in.ID, &rpcError{Code: -32601, Message: "method not found"})
	}
}

func (fc *fakeConn) write(v any) {
	fc.writeMu.Lock()
	defer fc.writeMu.Unlock()
	_ = fc.conn.WriteJSON(v)
}

func (fc *fakeConn) reply(id string, result any) {
	raw, _ := json.Marshal(result)
	fc.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(raw)})
}

// replyMaybeStalled answers id, optionally after delay. The delay runs in a
// goroutine so the serve loop keeps reading — the connection can still be torn
// down promptly while a stalled reply is pending.
func (fc *fakeConn) replyMaybeStalled(id string, delay time.Duration, result any) {
	if delay <= 0 {
		fc.reply(id, result)
		return
	}
	go func() {
		time.Sleep(delay)
		fc.reply(id, result)
	}()
}

func (fc *fakeConn) replyError(id string, e *rpcError) {
	fc.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
}

// emitEvent pushes one session.event notification to the client.
func (fc *fakeConn) emitEvent(sessionID, runID, eventID string, seq int64, event any) {
	evRaw, _ := json.Marshal(event)
	note := sessionEventNotification{
		SessionID: sessionID,
		RunID:     runID,
		EventID:   eventID,
		Seq:       seq,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Event:     evRaw,
	}
	noteRaw, _ := json.Marshal(note)
	fc.write(map[string]any{"jsonrpc": "2.0", "method": methodSessionEvent, "params": json.RawMessage(noteRaw)})
}

// testOptions builds a valid options value pointed at the fake server.
func testOptions(url string) options {
	return options{
		url:               url,
		token:             testToken,
		project:           "cc-connect",
		connectTimeout:    2 * time.Second,
		requestTimeout:    2 * time.Second,
		heartbeatInterval: time.Hour, // disabled by default in tests
		resume:            true,
	}
}
