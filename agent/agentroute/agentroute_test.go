package agentroute

import (
	"context"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestNew_RequiresURLAndToken(t *testing.T) {
	if _, err := New(map[string]any{"token": "secret"}); err == nil {
		t.Error("New without url should fail")
	}
	if _, err := New(map[string]any{"url": "wss://example.com/x"}); err == nil {
		t.Error("New without token should fail")
	}
}

func TestNew_RejectsInvalidURLScheme(t *testing.T) {
	if _, err := New(map[string]any{"url": "http://example.com/x", "token": "secret"}); err == nil {
		t.Error("New with a non-ws scheme should fail")
	}
}

func TestNew_SucceedsAndNames(t *testing.T) {
	ag, err := New(map[string]any{"url": "wss://example.com/x", "token": "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ag.Name() != "agentroute" {
		t.Errorf("Name() = %q, want agentroute", ag.Name())
	}
}

func TestAgent_RegisteredInRegistry(t *testing.T) {
	ag, err := core.CreateAgent("agentroute", map[string]any{"url": "wss://example.com/x", "token": "secret"})
	if err != nil {
		t.Fatalf("core.CreateAgent(agentroute): %v", err)
	}
	if ag.Name() != "agentroute" {
		t.Errorf("registered agent Name() = %q, want agentroute", ag.Name())
	}
}

func TestAgent_ImplementsOptionalInterfaces(t *testing.T) {
	ag, err := New(map[string]any{"url": "wss://example.com/x", "token": "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := ag.(core.SessionEnvInjector); !ok {
		t.Error("agentroute.Agent must implement core.SessionEnvInjector")
	}
	if _, ok := ag.(core.WorkspaceAgentOptionSnapshotter); !ok {
		t.Error("agentroute.Agent must implement core.WorkspaceAgentOptionSnapshotter")
	}
}

func TestStartSession_SendsCCConnectSessionIDFromSessionEnv(t *testing.T) {
	fs := newFakeServer(t)
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag.(core.SessionEnvInjector).SetSessionEnv([]string{
		"CC_PROJECT=cc-connect",
		"CC_SESSION_KEY=feishu:oc_x:ou_y",
	})

	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.CCConnectSessionID != "feishu:oc_x:ou_y" {
		t.Errorf("cc_connect_session_id = %q, want the CC_SESSION_KEY value", p.CCConnectSessionID)
	}
}

func TestStartSession_FallsBackToCCProjectFromSessionEnv(t *testing.T) {
	fs := newFakeServer(t)
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken}) // no "project" option
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag.(core.SessionEnvInjector).SetSessionEnv([]string{"CC_PROJECT=from-env"})

	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.Project != "from-env" {
		t.Errorf("project = %q, want fallback to CC_PROJECT", p.Project)
	}
}

func TestStartSession_DerivesWorkspaceFromWorkDir(t *testing.T) {
	fs := newFakeServer(t)
	// Mirror how the engine clones a per-workspace agent: only work_dir.
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken, "work_dir": "/srv/project-x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := ag.StartSession(context.Background(), "")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.Workspace != "/srv/project-x" {
		t.Errorf("session.start workspace = %q, want it derived from work_dir", p.Workspace)
	}
}

func TestStartSession_ResumesWithRouteSessionID(t *testing.T) {
	fs := newFakeServer(t)
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := ag.StartSession(context.Background(), "route_sess_prior")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	var p sessionStartParams
	fs.lastParams(t, methodSessionStart, &p)
	if p.ResumeSessionID != "route_sess_prior" {
		t.Errorf("resume_session_id = %q, want the route session id passed to StartSession", p.ResumeSessionID)
	}
}

func TestWorkspaceAgentOptions_CarriesURLTokenExcludesWorkDir(t *testing.T) {
	ag, err := New(map[string]any{
		"url":           "wss://example.com/x",
		"token":         "secret-token",
		"project":       "cc-connect",
		"default_agent": "codex",
		"work_dir":      "/srv/project-x",
		"workspace":     "github.com/ssc806/cc-connect",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := ag.(core.WorkspaceAgentOptionSnapshotter).WorkspaceAgentOptions()

	if snap["url"] != "wss://example.com/x" {
		t.Errorf("snapshot lost url: %v", snap["url"])
	}
	if snap["token"] != "secret-token" {
		t.Errorf("snapshot lost token: %v", snap["token"])
	}
	if _, ok := snap["work_dir"]; ok {
		t.Error("snapshot must exclude work_dir — the engine overrides it per workspace")
	}
	if _, ok := snap["workspace"]; ok {
		t.Error("snapshot must exclude workspace — the per-workspace agent derives it from work_dir")
	}

	// The snapshot must reconstruct a working agent.
	if _, err := New(snap); err != nil {
		t.Errorf("agent rebuilt from snapshot failed parseOptions: %v", err)
	}
}

func TestListSessions_MapsRemoteSessions(t *testing.T) {
	fs := newFakeServer(t)
	fs.listResult = sessionListResult{
		Sessions: []routeSessionInfo{
			{ID: "route_sess_1", AgentType: "codex", Summary: "Fix tests", MessageCount: 12, ModifiedAt: "2026-05-20T09:55:00Z"},
		},
	}
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	infos, err := ag.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(infos))
	}
	if infos[0].ID != "route_sess_1" || infos[0].Summary != "Fix tests" || infos[0].MessageCount != 12 {
		t.Errorf("session info not mapped: %+v", infos[0])
	}
}

func TestListSessions_FallsBackToCCProjectFromSessionEnv(t *testing.T) {
	fs := newFakeServer(t)
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken}) // no "project" option
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ag.(core.SessionEnvInjector).SetSessionEnv([]string{"CC_PROJECT=from-env"})

	if _, err := ag.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	// session.list must scope to the same project StartSession resolves, or a
	// session created under the CC_PROJECT fallback would be invisible to /list.
	var p sessionListParams
	fs.lastParams(t, methodSessionList, &p)
	if p.Project != "from-env" {
		t.Errorf("session.list project = %q, want fallback to CC_PROJECT", p.Project)
	}
}

func TestStartSession_ConnectErrorDoesNotLeakToken(t *testing.T) {
	fs := newFakeServer(t)
	fs.rejectAuth = true
	ag, err := New(map[string]any{"url": fs.dialURL(), "token": testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = ag.StartSession(context.Background(), "")
	if err == nil {
		t.Fatal("StartSession should fail when the server rejects auth")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("StartSession error leaked the token: %q", err.Error())
	}
}
