package agentroute

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// connectSession dials the fake server and starts a route session.
func connectSession(t *testing.T, fs *fakeServer, opts options, resumeID, ccKey string) *session {
	t.Helper()
	client, err := newRPCClient(context.Background(), opts)
	if err != nil {
		t.Fatalf("newRPCClient: %v", err)
	}
	s := newSession(opts, client, resumeID, ccKey)
	if err := s.start(context.Background()); err != nil {
		_ = client.Close()
		t.Fatalf("session.start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// readEvent reads one core.Event or fails the test.
func readEvent(t *testing.T, s *session) core.Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return core.Event{}
	}
}

// drainUntilTerminal collects events until a terminal one (result/error).
func drainUntilTerminal(t *testing.T, s *session) []core.Event {
	t.Helper()
	var out []core.Event
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
			if ev.Type == core.EventResult || ev.Type == core.EventError {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out; collected %d events", len(out))
		}
	}
}

func TestStartSession_CreatesRemoteSession(t *testing.T) {
	fs := newFakeServer(t)
	fs.sessionID = "route_sess_new"
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")

	if s.CurrentSessionID() != "route_sess_new" {
		t.Errorf("CurrentSessionID = %q, want route_sess_new", s.CurrentSessionID())
	}
	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.ResumeSessionID != "" {
		t.Errorf("fresh start should not carry resume_session_id, got %q", p.ResumeSessionID)
	}
}

func TestStartSession_ResumesRemoteSession(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "route_sess_prev", "")

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.ResumeSessionID != "route_sess_prev" {
		t.Errorf("resume_session_id = %q, want route_sess_prev", p.ResumeSessionID)
	}
	if s.CurrentSessionID() != "route_sess_prev" {
		t.Errorf("CurrentSessionID = %q, want resumed id", s.CurrentSessionID())
	}
}

func TestStartSession_OmitsResumeWhenDisabled(t *testing.T) {
	fs := newFakeServer(t)
	opts := testOptions(fs.dialURL())
	opts.resume = false
	connectSession(t, fs, opts, "route_sess_prev", "")

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.ResumeSessionID != "" {
		t.Errorf("resume disabled: resume_session_id should be empty, got %q", p.ResumeSessionID)
	}
}

func TestStartSession_SendsCCConnectSessionID(t *testing.T) {
	fs := newFakeServer(t)
	connectSession(t, fs, testOptions(fs.dialURL()), "", "feishu:oc_x:ou_y")

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.CCConnectSessionID != "feishu:oc_x:ou_y" {
		t.Errorf("cc_connect_session_id = %q, want feishu:oc_x:ou_y", p.CCConnectSessionID)
	}
}

func TestStartSession_SendsRequestID(t *testing.T) {
	fs := newFakeServer(t)
	connectSession(t, fs, testOptions(fs.dialURL()), "", "")

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.RequestID == "" {
		t.Error("session.start must carry a non-empty request_id")
	}
}

func TestStartSession_SendsWorkspace(t *testing.T) {
	fs := newFakeServer(t)
	opts := testOptions(fs.dialURL())
	opts.workspace = "github.com/ssc806/cc-connect"
	connectSession(t, fs, opts, "", "")

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.Workspace != "github.com/ssc806/cc-connect" {
		t.Errorf("session.start workspace = %q, want configured value", p.Workspace)
	}
}

func TestStartSession_UsesRequestTimeoutNotConnectTimeout(t *testing.T) {
	fs := newFakeServer(t)
	fs.startDelay = 1 * time.Second // longer than connect, shorter than request

	opts := testOptions(fs.dialURL())
	opts.connectTimeout = 300 * time.Millisecond
	opts.requestTimeout = 3 * time.Second

	// connectSession fails the test if session.start errors — a regression
	// that reused the 300ms connect budget would trip here.
	connectSession(t, fs, opts, "", "")
}

func TestSessionSend_SendsPromptAndAttachmentsShape(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")

	err := s.Send("review this PR",
		[]core.ImageAttachment{{MimeType: "image/png", Data: []byte("x"), FileName: "shot.png"}},
		[]core.FileAttachment{{MimeType: "text/plain", Data: []byte("log"), FileName: "err.log"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var p sessionSendParams
	fs.lastParams(t, methodSessionSend, &p)
	if p.Prompt != "review this PR" {
		t.Errorf("prompt = %q", p.Prompt)
	}
	if len(p.Images) != 1 || p.Images[0].Kind != "image" || p.Images[0].Name != "shot.png" {
		t.Errorf("image attachment shape wrong: %+v", p.Images)
	}
	if len(p.Files) != 1 || p.Files[0].Kind != "file" || p.Files[0].Name != "err.log" {
		t.Errorf("file attachment shape wrong: %+v", p.Files)
	}
}

func TestSessionSend_SendsRequestIDAndRunID(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")

	if err := s.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var send sessionSendParams
	fs.lastParams(t, methodSessionSend, &send)
	if send.RequestID == "" {
		t.Error("session.send must carry a non-empty request_id")
	}
	if send.RunID == "" {
		t.Fatal("session.send must carry a client-generated run_id")
	}

	// The client-generated run_id must become the active run.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var cancel sessionCancelParams
	fs.lastParams(t, methodSessionCancel, &cancel)
	if cancel.RunID != send.RunID {
		t.Errorf("session.cancel run_id = %q, want the active run %q", cancel.RunID, send.RunID)
	}
}

func TestSessionEvents_MapsTextDeltaAndResult(t *testing.T) {
	fs := newFakeServer(t)
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "evt_1", 1, protocolEvent{Type: "text_delta", Text: "working"})
		c.emitEvent(p.SessionID, p.RunID, "evt_2", 2, protocolEvent{Type: "result", Text: "done"})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	events := drainUntilTerminal(t, s)
	if events[0].Type != core.EventText || events[0].Content != "working" {
		t.Errorf("first event = %+v, want EventText", events[0])
	}
	last := events[len(events)-1]
	if last.Type != core.EventResult || last.Content != "done" {
		t.Errorf("last event = %+v, want EventResult", last)
	}
}

func TestSessionEvents_MapsToolStartAndToolResult(t *testing.T) {
	fs := newFakeServer(t)
	code := 0
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "e1", 1, protocolEvent{Type: "tool_start", Tool: "shell", InputSummary: "go test"})
		c.emitEvent(p.SessionID, p.RunID, "e2", 2, protocolEvent{Type: "tool_result", Tool: "shell", OutputSummary: "PASS", ExitCode: &code})
		c.emitEvent(p.SessionID, p.RunID, "e3", 3, protocolEvent{Type: "result", Text: "ok"})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	events := drainUntilTerminal(t, s)
	var sawToolUse, sawToolResult bool
	for _, ev := range events {
		if ev.Type == core.EventToolUse && ev.ToolName == "shell" {
			sawToolUse = true
		}
		if ev.Type == core.EventToolResult && ev.ToolResult == "PASS" {
			sawToolResult = true
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Errorf("expected tool_start->EventToolUse and tool_result->EventToolResult, got %+v", events)
	}
}

func TestSessionEvents_DeduplicatesEventID(t *testing.T) {
	fs := newFakeServer(t)
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "dup", 1, protocolEvent{Type: "text_delta", Text: "once"})
		c.emitEvent(p.SessionID, p.RunID, "dup", 1, protocolEvent{Type: "text_delta", Text: "once"})
		c.emitEvent(p.SessionID, p.RunID, "evt_final", 2, protocolEvent{Type: "result", Text: "done"})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	events := drainUntilTerminal(t, s)
	textCount := 0
	for _, ev := range events {
		if ev.Type == core.EventText {
			textCount++
		}
	}
	if textCount != 1 {
		t.Errorf("duplicate event_id should be dropped: got %d EventText, want 1", textCount)
	}
}

func TestTerminalError_EndsTurnAndClearsActiveRunID(t *testing.T) {
	fs := newFakeServer(t)
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "e1", 1, protocolEvent{
			Type: "error", ErrorCode: "runtime_disconnected", Message: "lost", Retryable: true, Terminal: true,
		})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	events := drainUntilTerminal(t, s)
	if last := events[len(events)-1]; last.Type != core.EventError {
		t.Fatalf("terminal error should produce EventError, got %+v", last)
	}
	// active run cleared -> Close issues no session.cancel
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := fs.count(methodSessionCancel); n != 0 {
		t.Errorf("terminal error should have cleared the active run; got %d session.cancel calls", n)
	}
}

func TestNonTerminalError_DoesNotEndTurnOrClearActiveRunID(t *testing.T) {
	fs := newFakeServer(t)
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "e1", 1, protocolEvent{
			Type: "error", ErrorCode: "rate_limited", Message: "slow down", Retryable: true, Terminal: false,
		})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := readEvent(t, s)
	if ev.Type == core.EventError {
		t.Fatal("non-terminal error must NOT map to EventError (it would end the turn)")
	}
	// run still active -> Close issues session.cancel
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := fs.count(methodSessionCancel); n != 1 {
		t.Errorf("non-terminal error must keep the run active; got %d session.cancel calls, want 1", n)
	}
}

func TestRespondPermission_UsesRunIDFromPermissionEvent(t *testing.T) {
	fs := newFakeServer(t)
	var runID string
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		runID = p.RunID
		c.emitEvent(p.SessionID, p.RunID, "e1", 1, protocolEvent{
			Type: "permission_request", PermissionRequestID: "perm_99", Tool: "shell", Description: "run tests",
		})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := readEvent(t, s)
	if ev.Type != core.EventPermissionRequest || ev.RequestID != "perm_99" {
		t.Fatalf("expected EventPermissionRequest with RequestID perm_99, got %+v", ev)
	}

	if err := s.RespondPermission("perm_99", core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	var p permissionRespondParams
	fs.lastParams(t, methodPermissionRespond, &p)
	if p.RunID != runID {
		t.Errorf("permission.respond run_id = %q, want the run that raised it %q", p.RunID, runID)
	}
	if p.PermissionRequestID != "perm_99" {
		t.Errorf("permission.respond permission_request_id = %q, want perm_99", p.PermissionRequestID)
	}
	if p.Behavior != "allow" {
		t.Errorf("permission.respond behavior = %q, want allow", p.Behavior)
	}
	if p.RequestID == "" {
		t.Error("permission.respond must carry a non-empty request_id")
	}
}

func TestRespondPermission_UnknownIDReturnsError(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	err := s.RespondPermission("never_seen", core.PermissionResult{Behavior: "allow"})
	if err == nil {
		t.Fatal("RespondPermission for an unknown id should return an error, not send an empty run_id")
	}
	if fs.count(methodPermissionRespond) != 0 {
		t.Error("no permission.respond should be sent when the run_id lookup misses")
	}
}

func TestClose_SendsCloseAndMarksNotAlive(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if !s.Alive() {
		t.Fatal("session should be alive after start")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.Alive() {
		t.Error("Alive() must be false after Close()")
	}
	if fs.count(methodSessionClose) != 1 {
		t.Errorf("Close should send exactly one session.close, got %d", fs.count(methodSessionClose))
	}
}

func TestClose_CancelsActiveRunID(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var send sessionSendParams
	fs.lastParams(t, methodSessionSend, &send)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fs.count(methodSessionCancel) != 1 {
		t.Fatalf("Close with an active run must send session.cancel, got %d", fs.count(methodSessionCancel))
	}
	var cancel sessionCancelParams
	fs.lastParams(t, methodSessionCancel, &cancel)
	if cancel.RunID != send.RunID {
		t.Errorf("session.cancel run_id = %q, want active run %q", cancel.RunID, send.RunID)
	}
}

func TestCloseAndCancel_SendRequestID(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var cancel sessionCancelParams
	fs.lastParams(t, methodSessionCancel, &cancel)
	if cancel.RequestID == "" {
		t.Error("session.cancel must carry a non-empty request_id")
	}
	var clse sessionCloseParams
	fs.lastParams(t, methodSessionClose, &clse)
	if clse.RequestID == "" {
		t.Error("session.close must carry a non-empty request_id")
	}
}

func TestTerminalEvent_ClearsActiveRunID(t *testing.T) {
	fs := newFakeServer(t)
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "e1", 1, protocolEvent{Type: "result", Text: "done"})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	drainUntilTerminal(t, s) // ensure the result event was processed

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := fs.count(methodSessionCancel); n != 0 {
		t.Errorf("a terminal result must clear the active run; got %d session.cancel calls", n)
	}
}

func TestConnectionLoss_MarksNotAliveAndEmitsError(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	fc := fs.waitConn(t)

	_ = fc.conn.Close() // abrupt server-side disconnect (not a graceful Close)

	// Alive() is not lazy — it flips to false as soon as the loss is seen,
	// so the engine recycles the state and reconnects via StartSession.
	deadline := time.Now().Add(2 * time.Second)
	for s.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Alive() {
		t.Fatal("Alive() must become false after the connection is lost")
	}

	// One EventError must be emitted so an in-flight turn ends cleanly.
	select {
	case ev := <-s.Events():
		if ev.Type != core.EventError {
			t.Errorf("expected EventError on connection loss, got %v", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection loss emitted no EventError")
	}
}

func TestSessionSend_NotAcceptedReturnsError(t *testing.T) {
	fs := newFakeServer(t)
	fs.rejectSend = true // server answers session.send with accepted=false
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")

	if err := s.Send("go", nil, nil); err == nil {
		t.Fatal("Send must return an error when session.send is not accepted; " +
			"otherwise the engine waits for events that never arrive")
	}
}

func TestRespondPermission_NotAcceptedKeepsCorrelation(t *testing.T) {
	fs := newFakeServer(t)
	fs.rejectPermission = true // server answers permission.respond with accepted=false
	fs.onSend = func(c *fakeConn, p sessionSendParams) {
		c.emitEvent(p.SessionID, p.RunID, "e1", 1, protocolEvent{
			Type: "permission_request", PermissionRequestID: "perm_1", Tool: "shell", Description: "run tests",
		})
	}
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ev := readEvent(t, s); ev.Type != core.EventPermissionRequest {
		t.Fatalf("expected EventPermissionRequest, got %+v", ev)
	}

	if err := s.RespondPermission("perm_1", core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("RespondPermission must return an error when the answer is not accepted")
	}

	// A rejected answer must leave the run_id correlation intact for a retry.
	s.mu.Lock()
	_, kept := s.permRunIDs["perm_1"]
	s.mu.Unlock()
	if !kept {
		t.Error("rejected permission.respond must keep the run_id correlation")
	}
}

func TestAlive_ReflectsClientFailureImmediately(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if !s.Alive() {
		t.Fatal("session should be alive after start")
	}

	// Failing the rpc client must be visible through Alive() at once, without
	// waiting for the pump goroutine to observe the disconnect.
	s.client.fail(errors.New("simulated connection failure"))
	if s.Alive() {
		t.Fatal("Alive() must be false as soon as the rpc client fails")
	}
}

func TestClose_BoundsRPCsWhenServerStalls(t *testing.T) {
	defer func(orig time.Duration) { closeRPCTimeout = orig }(closeRPCTimeout)
	closeRPCTimeout = 200 * time.Millisecond

	fs := newFakeServer(t)
	fs.stallCancelClose = 3 * time.Second // far longer than the close budget
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("go", nil, nil); err != nil { // gives Close an active run to cancel
		t.Fatalf("Send: %v", err)
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		_ = s.Close()
		close(done)
	}()
	select {
	case <-done:
		// cancel + close are each bounded by closeRPCTimeout.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("Close took %s; it must bound its RPCs, not wait on the stalled server", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on the stalled server instead of bounding its cancel/close RPCs")
	}
}

func TestRunAffectingRequests_CarryUniqueRequestIDs(t *testing.T) {
	fs := newFakeServer(t)
	s := connectSession(t, fs, testOptions(fs.dialURL()), "", "")
	if err := s.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	seen := map[string]bool{}
	for _, method := range []string{methodSessionStart, methodSessionSend, methodSessionCancel, methodSessionClose} {
		for _, raw := range fs.allParams(t, method) {
			var p struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("decode %s params: %v", method, err)
			}
			if p.RequestID == "" {
				t.Errorf("%s carried an empty request_id", method)
			}
			if seen[p.RequestID] {
				t.Errorf("request_id %q reused across run-affecting requests", p.RequestID)
			}
			seen[p.RequestID] = true
		}
	}
}
