package agentroute

import (
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// mapEvent converts one protocol event object (§8.1) into a core.Event.
//
// It returns:
//   - ev:       the core.Event to forward (valid only when emit is true)
//   - emit:     whether the event should reach core.Engine at all
//   - terminal: whether the event ends the turn; the caller clears the
//     active run_id on a terminal event (see session.go step 1)
//
// The terminal/non-terminal split for "error" matters: core.EventError makes
// the Engine finalize the turn as failed, so a non-terminal protocol error
// must never map to EventError (implementation plan §5 mapping.go).
func mapEvent(pe protocolEvent) (ev core.Event, emit bool, terminal bool) {
	switch pe.Type {
	case "text_delta":
		return core.Event{Type: core.EventText, Content: pe.Text}, true, false

	case "thinking_delta":
		return core.Event{Type: core.EventThinking, Content: pe.Text}, true, false

	case "status":
		msg := strings.TrimSpace(firstNonEmpty(pe.Message, pe.Status))
		if msg == "" {
			return core.Event{}, false, false
		}
		return core.Event{Type: core.EventThinking, Content: msg}, true, false

	case "tool_start":
		return core.Event{
			Type:      core.EventToolUse,
			ToolName:  pe.Tool,
			ToolInput: pe.InputSummary,
		}, true, false

	case "tool_result":
		ev := core.Event{
			Type:         core.EventToolResult,
			ToolName:     pe.Tool,
			ToolResult:   pe.OutputSummary,
			ToolExitCode: pe.ExitCode,
		}
		if pe.ExitCode != nil {
			ok := *pe.ExitCode == 0
			ev.ToolSuccess = &ok
		}
		return ev, true, false

	case "permission_request":
		return core.Event{
			Type:         core.EventPermissionRequest,
			ToolName:     pe.Tool,
			ToolInput:    pe.Description,
			ToolInputRaw: pe.Input,
			// RequestID carries the protocol permission_request_id so the
			// Engine echoes it back into RespondPermission.
			RequestID: pe.PermissionRequestID,
		}, true, false

	case "artifact":
		// First milestone: object storage is out of scope (plan §2). Fold a
		// short reference into a text event so the user is not left unaware.
		name := "artifact"
		if pe.Artifact != nil && pe.Artifact.Name != "" {
			name = pe.Artifact.Name
		}
		return core.Event{Type: core.EventText, Content: "📎 artifact: " + name}, true, false

	case "result":
		return core.Event{
			Type:      core.EventResult,
			Content:   pe.Text,
			SessionID: pe.SessionID,
		}, true, true

	case "cancelled":
		msg := "run cancelled"
		if pe.Reason != "" {
			msg = "run cancelled: " + pe.Reason
		}
		return core.Event{Type: core.EventResult, Content: msg}, true, true

	case "error":
		perr := &ProtocolError{Code: pe.ErrorCode, Retryable: pe.Retryable, Message: pe.Message}
		if pe.Terminal {
			return core.Event{Type: core.EventError, Error: perr}, true, true
		}
		// Non-terminal error: surface it as a thinking status line so the
		// turn keeps running. It must NOT become EventError.
		return core.Event{Type: core.EventThinking, Content: "⚠️ " + perr.Error()}, true, false

	default:
		// Unknown / future event type — ignore at debug level.
		return core.Event{}, false, false
	}
}

// attachmentRefsFromImages converts inbound images into metadata-only
// protocol refs. The first milestone carries no object-storage URL — the
// shape is kept ready for a later milestone (plan §2).
func attachmentRefsFromImages(imgs []core.ImageAttachment) []attachmentRef {
	if len(imgs) == 0 {
		return nil
	}
	refs := make([]attachmentRef, 0, len(imgs))
	for _, img := range imgs {
		refs = append(refs, attachmentRef{
			Kind:      "image",
			Name:      img.FileName,
			MimeType:  img.MimeType,
			SizeBytes: int64(len(img.Data)),
		})
	}
	return refs
}

// attachmentRefsFromFiles converts inbound files into metadata-only refs.
func attachmentRefsFromFiles(files []core.FileAttachment) []attachmentRef {
	if len(files) == 0 {
		return nil
	}
	refs := make([]attachmentRef, 0, len(files))
	for _, f := range files {
		refs = append(refs, attachmentRef{
			Kind:      "file",
			Name:      f.FileName,
			MimeType:  f.MimeType,
			SizeBytes: int64(len(f.Data)),
		})
	}
	return refs
}

// mapPermissionResult maps a core.PermissionResult to the permission.respond
// fields it owns. run_id, session_id, permission_request_id and the
// request_id idempotency key are supplied separately by session.go.
func mapPermissionResult(r core.PermissionResult) (behavior string, updatedInput map[string]any, message string) {
	return r.Behavior, r.UpdatedInput, r.Message
}

// toAgentSessionInfo converts a remote session.list entry into the
// engine-facing core.AgentSessionInfo.
func toAgentSessionInfo(s routeSessionInfo) core.AgentSessionInfo {
	modified, _ := time.Parse(time.RFC3339, s.ModifiedAt)
	return core.AgentSessionInfo{
		ID:           s.ID,
		Summary:      s.Summary,
		MessageCount: s.MessageCount,
		ModifiedAt:   modified,
	}
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
