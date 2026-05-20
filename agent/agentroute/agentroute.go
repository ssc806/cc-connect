// Package agentroute is a cc-connect agent adapter that uses a cloud
// agent-route service as a remote core.Agent.
//
// The adapter speaks the agent-route WebSocket + JSON-RPC 2.0 protocol.
// cc-connect keeps all IM platform handling and local user interaction;
// agent-route owns cloud session management, agent routing, provisioning,
// and runtime fan-out.
//
// The adapter adds no agent-route special cases to core.Engine. It opts into
// the generic optional interfaces core already defines — SessionEnvInjector
// (to capture the local session key) and WorkspaceAgentOptionSnapshotter (to
// survive multi-workspace mode).
package agentroute

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("agentroute", New)
}

// Agent implements core.Agent for the cloud agent-route service.
type Agent struct {
	opts options

	mu           sync.Mutex
	ccSessionKey string // captured from CC_SESSION_KEY via SetSessionEnv
	ccProject    string // captured from CC_PROJECT via SetSessionEnv
}

// New builds an agentroute Agent from project options.
func New(raw map[string]any) (core.Agent, error) {
	opts, err := parseOptions(raw)
	if err != nil {
		return nil, err
	}
	return &Agent{opts: opts}, nil
}

// Name returns the stable agent type identifier used in config, session store
// keys, and audit logging.
func (a *Agent) Name() string { return "agentroute" }

// Stop releases agent-level resources. Each session owns its own connection,
// so there is nothing to tear down here.
func (a *Agent) Stop() error { return nil }

// SetSessionEnv implements core.SessionEnvInjector. The engine calls it before
// StartSession with CC_SESSION_KEY / CC_PROJECT / ... entries. CC_SESSION_KEY
// becomes the protocol's cc_connect_session_id; CC_PROJECT is a fallback for
// the project param when no project option was configured.
func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CC_SESSION_KEY="); ok {
			a.ccSessionKey = v
		}
		if v, ok := strings.CutPrefix(kv, "CC_PROJECT="); ok {
			a.ccProject = v
		}
	}
}

// WorkspaceAgentOptions implements core.WorkspaceAgentOptionSnapshotter.
//
// In multi-workspace mode the engine builds a fresh options map per workspace
// and calls CreateAgent again; that map is seeded only from this snapshot plus
// a few engine-copied keys (work_dir, model, mode, run_as_*). Because
// parseOptions requires url and token, the snapshot must carry everything
// needed to reconstruct the adapter.
//
// work_dir and workspace are deliberately excluded: the engine overrides
// work_dir per workspace, and parseOptions derives workspace from work_dir.
func (a *Agent) WorkspaceAgentOptions() map[string]any {
	return map[string]any{
		"url":                     a.opts.url,
		"token":                   a.opts.token,
		"project":                 a.opts.project,
		"default_agent":           a.opts.defaultAgent,
		"connect_timeout_secs":    int64(a.opts.connectTimeout / time.Second),
		"request_timeout_secs":    int64(a.opts.requestTimeout / time.Second),
		"heartbeat_interval_secs": int64(a.opts.heartbeatInterval / time.Second),
		"resume":                  a.opts.resume,
		"allow_insecure_ws":       a.opts.allowInsecureWS,
	}
}

// effectiveOptions returns a.opts with the §3 default rule applied: when no
// project was configured, the CC_PROJECT value captured from the session env
// is used. StartSession and ListSessions must resolve the project the same
// way — otherwise a session created under the CC_PROJECT fallback would be
// invisible to a /list query that sent an empty project.
func (a *Agent) effectiveOptions() options {
	a.mu.Lock()
	ccProject := a.ccProject
	a.mu.Unlock()

	opts := a.opts
	if opts.project == "" && ccProject != "" {
		opts.project = ccProject
	}
	return opts
}

// StartSession creates or resumes a route session. sessionID is the
// agent-route session_id the engine persisted; it is used as
// resume_session_id (it is NOT the local cc_connect_session_id).
func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	ccSessionKey := a.ccSessionKey
	a.mu.Unlock()

	opts := a.effectiveOptions()

	client, err := newRPCClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("agentroute: connect: %w", err)
	}
	s := newSession(opts, client, sessionID, ccSessionKey)
	if err := s.start(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return s, nil
}

// ListSessions returns the route sessions visible to the authenticated
// cc-connect project. It opens a short-lived connection for the query.
func (a *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	opts := a.effectiveOptions()
	client, err := newRPCClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("agentroute: connect: %w", err)
	}
	defer client.Close()

	params := sessionListParams{
		Project:   opts.project,
		Workspace: opts.workspace,
		Limit:     50,
	}
	var res sessionListResult
	if err := client.call(ctx, methodSessionList, params, &res); err != nil {
		return nil, fmt.Errorf("agentroute: session.list: %w", err)
	}

	infos := make([]core.AgentSessionInfo, 0, len(res.Sessions))
	for _, si := range res.Sessions {
		infos = append(infos, toAgentSessionInfo(si))
	}
	return infos, nil
}
