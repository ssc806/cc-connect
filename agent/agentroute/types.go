package agentroute

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// Protocol identity for the agent-route JSON-RPC 2.0 protocol.
const (
	protocolName    = "agentroute-jsonrpc"
	protocolVersion = 1
	// protocolHeader is the value of the required X-AgentRoute-Protocol
	// handshake header.
	protocolHeader = "agentroute-jsonrpc/1"
)

// JSON-RPC method names (§7, §8).
const (
	methodConnectionHello   = "connection.hello"
	methodSessionStart      = "session.start"
	methodSessionSend       = "session.send"
	methodPermissionRespond = "permission.respond"
	methodSessionCancel     = "session.cancel"
	methodSessionClose      = "session.close"
	methodSessionList       = "session.list"
	methodPing              = "ping"
	methodSessionEvent      = "session.event"
)

// ---- JSON-RPC 2.0 envelope (§4) ----

// rpcRequest is an outbound JSON-RPC request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcErrorData carries the stable application error fields from §4/§9.
type rpcErrorData struct {
	ErrorCode string `json:"error_code"`
	Retryable bool   `json:"retryable"`
	SessionID string `json:"session_id,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *rpcErrorData `json:"data,omitempty"`
}

// rpcIncoming is a frame read from the server. It is either a response
// (non-empty ID, plus Result or Error) or a notification (non-empty Method,
// empty ID).
type rpcIncoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcResponse is the correlated result delivered to a waiting caller.
type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

// ---- connection.hello (§7.1) ----

type helloClient struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	AgentAdapter string `json:"agent_adapter"`
}

type helloParams struct {
	Protocol     string      `json:"protocol"`
	Version      int         `json:"version"`
	Client       helloClient `json:"client"`
	Capabilities []string    `json:"capabilities"`
}

type helloResult struct {
	ConnectionID string   `json:"connection_id"`
	Protocol     string   `json:"protocol"`
	Version      int      `json:"version"`
	ServerTime   string   `json:"server_time"`
	Capabilities []string `json:"capabilities"`
}

// ---- attachment ref (§6.2) ----

// attachmentRef is the small metadata-only attachment shape. The first
// milestone sends empty image/file arrays but keeps the type ready.
type attachmentRef struct {
	ID        string `json:"id,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// ---- session.start (§7.2) ----

type sessionStartParams struct {
	RequestID          string         `json:"request_id"`
	ResumeSessionID    string         `json:"resume_session_id,omitempty"`
	CCConnectSessionID string         `json:"cc_connect_session_id"`
	Project            string         `json:"project,omitempty"`
	Workspace          string         `json:"workspace,omitempty"`
	DefaultAgent       string         `json:"default_agent,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type sessionStartResult struct {
	SessionID          string `json:"session_id"`
	CCConnectSessionID string `json:"cc_connect_session_id"`
	AgentID            string `json:"agent_id"`
	AgentType          string `json:"agent_type"`
	Status             string `json:"status"`
	Recoverable        bool   `json:"recoverable"`
}

// ---- session.send (§7.3) ----

type sessionSendParams struct {
	RequestID string          `json:"request_id"`
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id"`
	Prompt    string          `json:"prompt"`
	Images    []attachmentRef `json:"images"`
	Files     []attachmentRef `json:"files"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

type sessionSendResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Accepted  bool   `json:"accepted"`
	Status    string `json:"status"`
}

// ---- permission.respond (§7.4) ----

type permissionRespondParams struct {
	RequestID           string         `json:"request_id"`
	SessionID           string         `json:"session_id"`
	RunID               string         `json:"run_id"`
	PermissionRequestID string         `json:"permission_request_id"`
	Behavior            string         `json:"behavior"`
	UpdatedInput        map[string]any `json:"updated_input,omitempty"`
	Message             string         `json:"message,omitempty"`
}

type permissionRespondResult struct {
	Accepted bool `json:"accepted"`
}

// ---- session.cancel (§7.5) ----

type sessionCancelParams struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	Reason    string `json:"reason,omitempty"`
}

type sessionCancelResult struct {
	Accepted bool   `json:"accepted"`
	Status   string `json:"status"`
}

// ---- session.close (§7.6) ----

type sessionCloseParams struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

type sessionCloseResult struct {
	Closed bool `json:"closed"`
}

// ---- session.list (§7.7) ----

type sessionListParams struct {
	Project   string `json:"project,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}

type routeSessionInfo struct {
	ID           string `json:"id"`
	AgentType    string `json:"agent_type"`
	Summary      string `json:"summary"`
	MessageCount int    `json:"message_count"`
	ModifiedAt   string `json:"modified_at"`
}

type sessionListResult struct {
	Sessions   []routeSessionInfo `json:"sessions"`
	NextCursor string             `json:"next_cursor"`
}

// ---- ping (§7.8) ----

type pingParams struct {
	TS string `json:"ts"`
}

type pingResult struct {
	TS         string `json:"ts"`
	ServerTime string `json:"server_time"`
}

// ---- session.event notification (§8) ----

// sessionEventNotification is the common wrapper carried by every
// session.event notification. Every wrapper exposes session_id, run_id,
// event_id and seq so session.go can correlate runs and deduplicate.
type sessionEventNotification struct {
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id"`
	EventID   string          `json:"event_id"`
	Seq       int64           `json:"seq"`
	CreatedAt string          `json:"created_at"`
	Event     json.RawMessage `json:"event"`
}

// protocolEvent is the flattened union of every event object the protocol may
// embed (§8.1). Unknown fields are ignored and absent fields stay zero.
type protocolEvent struct {
	Type string `json:"type"`

	// text_delta / thinking_delta / result
	Text string `json:"text"`

	// status
	Status string `json:"status"`

	// status / error / generic
	Message string `json:"message"`

	// tool_start / tool_result
	Tool          string `json:"tool"`
	InputSummary  string `json:"input_summary"`
	OutputSummary string `json:"output_summary"`
	ExitCode      *int   `json:"exit_code"`

	// permission_request
	PermissionRequestID string         `json:"permission_request_id"`
	Description         string         `json:"description"`
	Input               map[string]any `json:"input"`

	// error
	ErrorCode string `json:"error_code"`
	Retryable bool   `json:"retryable"`
	Terminal  bool   `json:"terminal"`

	// cancelled
	Reason string `json:"reason"`

	// result
	SessionID string `json:"session_id"`

	// artifact
	Artifact *attachmentRef `json:"artifact"`
}

// ---- idempotency / turn identity helpers ----

// newRequestID generates a fresh idempotency key. Every run-affecting request
// (§2) carries one; a transparent retry of the same logical attempt must reuse
// the value rather than minting a new one.
func newRequestID() string { return "idem_" + randomToken() }

// newRunID generates a fresh turn identity. This adapter takes the
// client-generates side for run_id (§5) so the active run is known
// deterministically before the session.send response arrives.
func newRunID() string { return "run_" + randomToken() }

// newEnvelopeID generates a JSON-RPC envelope id, used only for
// request/response correlation and distinct from request_id.
func newEnvelopeID() string { return "rpc_" + randomToken() }

// randomToken returns 16 random bytes hex-encoded. crypto/rand.Read never
// fails on supported platforms; a zero token is still acceptable here because
// these IDs only need to be unique within one connection.
func randomToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
