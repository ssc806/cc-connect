//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/chenhg5/cc-connect/core"

	// Registers the "agentroute" agent in core's registry via init().
	_ "github.com/chenhg5/cc-connect/agent/agentroute"
)

// These tests exercise the agentroute adapter end-to-end against a real
// in-process WebSocket server: a full Platform -> Engine -> Agent -> WebSocket
// -> server -> Agent -> Engine -> Platform round trip. Unlike the in-package
// agentroute tests, the adapter is driven only through core's public Agent /
// Engine surface and the fake server is built from raw JSON-RPC frames, so it
// never shares the adapter's own protocol structs.

// ---------------------------------------------------------------------------
// fakeAgentRoute — a minimal agent-route JSON-RPC WebSocket server.
// ---------------------------------------------------------------------------

type fakeAgentRoute struct {
	srv *httptest.Server

	mu          sync.Mutex
	sendPrompts []string         // prompts received via session.send
	listResult  []map[string]any // sessions returned by session.list
}

var fakeARUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func newFakeAgentRoute(t *testing.T) *fakeAgentRoute {
	t.Helper()
	f := &fakeAgentRoute{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// wsURL is the loopback ws:// endpoint the adapter dials. Loopback keeps the
// adapter's wss://-for-remote-hosts rule satisfied without allow_insecure_ws.
func (f *fakeAgentRoute) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/agent-sessions"
}

func (f *fakeAgentRoute) setListResult(sessions []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listResult = sessions
}

func (f *fakeAgentRoute) getPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sendPrompts))
	copy(out, f.sendPrompts)
	return out
}

func (f *fakeAgentRoute) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := fakeARUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var writeMu sync.Mutex
	send := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(v)
	}

	for {
		var in struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := conn.ReadJSON(&in); err != nil {
			return // client closed or connection lost
		}
		f.dispatch(send, in.ID, in.Method, in.Params)
	}
}

func (f *fakeAgentRoute) dispatch(send func(any), id, method string, params json.RawMessage) {
	reply := func(result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	now := time.Now().UTC().Format(time.RFC3339)

	switch method {
	case "connection.hello":
		// Protocol/version must match what the adapter speaks or it rejects
		// the connection (the hello compatibility gate).
		reply(map[string]any{
			"connection_id": "conn_e2e",
			"protocol":      "agentroute-jsonrpc",
			"version":       1,
			"server_time":   now,
			"capabilities":  []string{"text", "stream", "permission", "cancel", "session_list"},
		})

	case "session.start":
		var p struct {
			ResumeSessionID    string `json:"resume_session_id"`
			CCConnectSessionID string `json:"cc_connect_session_id"`
		}
		_ = json.Unmarshal(params, &p)
		sid := p.ResumeSessionID
		if sid == "" {
			sid = "route_sess_e2e"
		}
		reply(map[string]any{
			"session_id":            sid,
			"cc_connect_session_id": p.CCConnectSessionID,
			"agent_type":            "codex",
			"status":                "ready",
		})

	case "session.send":
		var p struct {
			SessionID string `json:"session_id"`
			RunID     string `json:"run_id"`
			Prompt    string `json:"prompt"`
		}
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		f.sendPrompts = append(f.sendPrompts, p.Prompt)
		f.mu.Unlock()
		reply(map[string]any{"session_id": p.SessionID, "run_id": p.RunID, "accepted": true, "status": "running"})
		// Stream one turn: a text delta carrying a recognisable marker, then a
		// terminal result that ends the turn.
		f.emitEvent(send, p.SessionID, p.RunID, "evt_text", 1,
			map[string]any{"type": "text_delta", "text": "AGENTROUTE_E2E_REPLY_OK"})
		f.emitEvent(send, p.SessionID, p.RunID, "evt_result", 2,
			map[string]any{"type": "result", "text": ""})

	case "session.list":
		f.mu.Lock()
		sessions := f.listResult
		f.mu.Unlock()
		reply(map[string]any{"sessions": sessions, "next_cursor": ""})

	case "permission.respond":
		reply(map[string]any{"accepted": true})
	case "session.cancel":
		reply(map[string]any{"accepted": true, "status": "cancelling"})
	case "session.close":
		reply(map[string]any{"closed": true})
	case "ping":
		reply(map[string]any{"ts": now, "server_time": now})
	default:
		send(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

func (f *fakeAgentRoute) emitEvent(send func(any), sessionID, runID, eventID string, seq int64, event map[string]any) {
	send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session.event",
		"params": map[string]any{
			"session_id": sessionID,
			"run_id":     runID,
			"event_id":   eventID,
			"seq":        seq,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"event":      event,
		},
	})
}

// ---------------------------------------------------------------------------
// T-320: Engine drives the agentroute agent over a real WebSocket round trip.
// ---------------------------------------------------------------------------

func TestIntegration_AgentRoute_EngineMessageFlow(t *testing.T) {
	f := newFakeAgentRoute(t)
	workDir := t.TempDir()

	agent, err := core.CreateAgent("agentroute", map[string]any{
		"url":      f.wsURL(),
		"token":    "e2e-token",
		"work_dir": workDir,
	})
	if err != nil {
		t.Fatalf("core.CreateAgent(agentroute): %v", err)
	}

	mp := &mockPlatform{agent: agent}
	e := core.NewEngine("agentroute-e2e", agent, []core.Platform{mp},
		filepath.Join(workDir, "sessions.json"), core.LangEnglish)
	defer func() { agent.Stop(); e.Stop() }()

	e.ReceiveMessage(mp, &core.Message{
		SessionKey: sessionKey("ar-user"),
		Platform:   "mock",
		UserID:     "ar-user",
		UserName:   "tester",
		Content:    "diagnose AGENTROUTE_PROMPT_MARKER please",
		ReplyCtx:   "ctx",
	})

	reply, ok := waitForMessageContaining(mp, "AGENTROUTE_E2E_REPLY_OK", 20*time.Second)
	if !ok {
		t.Fatalf("engine never delivered the agent-route reply; platform got: %v", mp.getSent())
	}
	t.Logf("platform received reply: %.80s", reply)

	// The prompt must have travelled Platform -> Engine -> Agent -> WebSocket.
	prompts := f.getPrompts()
	if len(prompts) == 0 {
		t.Fatal("agent-route server received no session.send")
	}
	if !strings.Contains(prompts[0], "AGENTROUTE_PROMPT_MARKER") {
		t.Errorf("server prompt %q does not carry the user message", prompts[0])
	}
}

// ---------------------------------------------------------------------------
// T-321: ListSessions through the registry-created agent over a real socket.
// ---------------------------------------------------------------------------

func TestIntegration_AgentRoute_ListSessionsViaRegistry(t *testing.T) {
	f := newFakeAgentRoute(t)
	f.setListResult([]map[string]any{
		{
			"id": "route_sess_1", "agent_type": "codex", "summary": "Fix the parser",
			"message_count": 7, "modified_at": "2026-05-20T09:00:00Z",
		},
	})

	agent, err := core.CreateAgent("agentroute", map[string]any{
		"url":   f.wsURL(),
		"token": "e2e-token",
	})
	if err != nil {
		t.Fatalf("core.CreateAgent(agentroute): %v", err)
	}
	defer agent.Stop()

	infos, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(infos))
	}
	if infos[0].ID != "route_sess_1" || infos[0].Summary != "Fix the parser" || infos[0].MessageCount != 7 {
		t.Errorf("session info mapped wrong: %+v", infos[0])
	}
}

// ---------------------------------------------------------------------------
// T-322: A per-workspace agent rebuilt from the WorkspaceAgentOptions snapshot
// is not just parseable — it can run a full turn against the server. This
// mirrors how the engine clones an agent per workspace (CreateAgent from the
// snapshot map plus an engine-supplied work_dir).
// ---------------------------------------------------------------------------

func TestIntegration_AgentRoute_WorkspaceSnapshotReclone(t *testing.T) {
	f := newFakeAgentRoute(t)

	base, err := core.CreateAgent("agentroute", map[string]any{
		"url":           f.wsURL(),
		"token":         "e2e-token",
		"project":       "cc-connect",
		"default_agent": "codex",
	})
	if err != nil {
		t.Fatalf("core.CreateAgent(agentroute): %v", err)
	}
	defer base.Stop()

	snapshotter, ok := base.(core.WorkspaceAgentOptionSnapshotter)
	if !ok {
		t.Fatal("agentroute agent must implement core.WorkspaceAgentOptionSnapshotter")
	}
	snap := snapshotter.WorkspaceAgentOptions()
	snap["work_dir"] = t.TempDir() // the engine overrides work_dir per workspace

	cloned, err := core.CreateAgent("agentroute", snap)
	if err != nil {
		t.Fatalf("rebuilding the agent from its workspace snapshot failed: %v", err)
	}
	defer cloned.Stop()

	sess, err := cloned.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("cloned agent StartSession: %v", err)
	}
	defer sess.Close()

	if err := sess.Send("hello from the cloned workspace agent", nil, nil); err != nil {
		t.Fatalf("cloned agent Send: %v", err)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev := <-sess.Events():
			if ev.Type == core.EventError {
				t.Fatalf("cloned session emitted EventError: %v", ev.Error)
			}
			if ev.Type == core.EventResult {
				return // a full turn completed against the server — clone works
			}
		case <-deadline:
			t.Fatal("cloned session produced no terminal result")
		}
	}
}
