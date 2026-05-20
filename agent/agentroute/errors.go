package agentroute

import (
	"fmt"
	"regexp"

	"github.com/chenhg5/cc-connect/core"
)

// ProtocolError is a typed agent-route error. It preserves the stable
// application error_code and the retryable hint from the protocol so callers
// can branch on them, while keeping the human-readable message English-only
// (the engine localizes the "❌ Error:" prefix — see CLAUDE.md §7).
type ProtocolError struct {
	RPCCode   int    // JSON-RPC numeric code (e.g. -32010)
	Code      string // stable error.data.error_code (e.g. "session_busy")
	Retryable bool   // error.data.retryable
	Message   string // human-readable detail, not part of program logic
}

func (e *ProtocolError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("agentroute: %s: %s", e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("agentroute: %s", e.Code)
	case e.Message != "":
		return fmt.Sprintf("agentroute: rpc error %d: %s", e.RPCCode, e.Message)
	default:
		return fmt.Sprintf("agentroute: rpc error %d", e.RPCCode)
	}
}

// signedURLQuery matches the query string of an http(s) URL. Signed object
// storage URLs carry credentials there, so the query is redacted wholesale.
var signedURLQuery = regexp.MustCompile(`(https?://[^\s?]+)\?[^\s]*`)

// redactSecrets scrubs a string before it reaches a log line or an error
// returned to the engine: it removes the bearer token and strips the query
// string of any embedded http(s) URL (signed attachment URLs).
func redactSecrets(text, token string) string {
	text = core.RedactToken(text, token)
	return signedURLQuery.ReplaceAllString(text, "$1?[REDACTED]")
}
