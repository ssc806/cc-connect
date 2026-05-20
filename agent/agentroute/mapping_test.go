package agentroute

import (
	"errors"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestMapEvent_TextDelta(t *testing.T) {
	ev, emit, terminal := mapEvent(protocolEvent{Type: "text_delta", Text: "hello world"})
	if !emit {
		t.Fatal("text_delta should be emitted")
	}
	if terminal {
		t.Error("text_delta is not terminal")
	}
	if ev.Type != core.EventText || ev.Content != "hello world" {
		t.Errorf("got %+v, want EventText with content", ev)
	}
}

func TestMapEvent_ThinkingDelta(t *testing.T) {
	ev, emit, _ := mapEvent(protocolEvent{Type: "thinking_delta", Text: "pondering"})
	if !emit || ev.Type != core.EventThinking || ev.Content != "pondering" {
		t.Errorf("got emit=%v %+v, want EventThinking", emit, ev)
	}
}

func TestMapEvent_ToolStart(t *testing.T) {
	ev, emit, _ := mapEvent(protocolEvent{Type: "tool_start", Tool: "shell", InputSummary: "go test ./core"})
	if !emit || ev.Type != core.EventToolUse {
		t.Fatalf("got emit=%v type=%v, want EventToolUse", emit, ev.Type)
	}
	if ev.ToolName != "shell" || ev.ToolInput != "go test ./core" {
		t.Errorf("tool_start mapping lost fields: %+v", ev)
	}
}

func TestMapEvent_ToolResult(t *testing.T) {
	code := 0
	ev, emit, _ := mapEvent(protocolEvent{Type: "tool_result", Tool: "shell", OutputSummary: "PASS", ExitCode: &code})
	if !emit || ev.Type != core.EventToolResult {
		t.Fatalf("got emit=%v type=%v, want EventToolResult", emit, ev.Type)
	}
	if ev.ToolResult != "PASS" {
		t.Errorf("tool_result lost output_summary: %+v", ev)
	}
	if ev.ToolExitCode == nil || *ev.ToolExitCode != 0 {
		t.Errorf("tool_result lost exit_code: %+v", ev.ToolExitCode)
	}
}

func TestMapEvent_Result(t *testing.T) {
	ev, emit, terminal := mapEvent(protocolEvent{Type: "result", Text: "done"})
	if !emit || !terminal {
		t.Fatalf("result must be emitted and terminal, got emit=%v terminal=%v", emit, terminal)
	}
	if ev.Type != core.EventResult || ev.Content != "done" {
		t.Errorf("got %+v, want EventResult with content", ev)
	}
}

func TestMapEvent_TerminalError(t *testing.T) {
	ev, emit, terminal := mapEvent(protocolEvent{
		Type:      "error",
		ErrorCode: "runtime_disconnected",
		Message:   "agent runtime disconnected",
		Retryable: true,
		Terminal:  true,
	})
	if !emit || !terminal {
		t.Fatalf("terminal error must be emitted and terminal, got emit=%v terminal=%v", emit, terminal)
	}
	if ev.Type != core.EventError {
		t.Fatalf("terminal error must map to EventError, got %v", ev.Type)
	}
	if ev.Error == nil {
		t.Fatal("EventError must carry a non-nil Error")
	}
	var pe *ProtocolError
	if !errors.As(ev.Error, &pe) {
		t.Fatalf("EventError.Error should be a *ProtocolError, got %T", ev.Error)
	}
	if pe.Code != "runtime_disconnected" || !pe.Retryable {
		t.Errorf("ProtocolError lost error_code/retryable: %+v", pe)
	}
}

func TestMapEvent_NonTerminalError(t *testing.T) {
	ev, _, terminal := mapEvent(protocolEvent{
		Type:      "error",
		ErrorCode: "rate_limited",
		Message:   "slow down",
		Retryable: true,
		Terminal:  false,
	})
	if terminal {
		t.Error("non-terminal error must not be terminal")
	}
	if ev.Type == core.EventError {
		t.Error("non-terminal error must NOT map to core.EventError — it would end the turn")
	}
}

func TestMapEvent_PermissionRequest(t *testing.T) {
	ev, emit, terminal := mapEvent(protocolEvent{
		Type:                "permission_request",
		PermissionRequestID: "perm_abc",
		Tool:                "shell",
		Description:         "Run tests",
		Input:               map[string]any{"cmd": "go test ./..."},
	})
	if !emit || terminal {
		t.Fatalf("permission_request: emit=%v terminal=%v", emit, terminal)
	}
	if ev.Type != core.EventPermissionRequest {
		t.Fatalf("got %v, want EventPermissionRequest", ev.Type)
	}
	if ev.RequestID != "perm_abc" {
		t.Errorf("RequestID = %q, want the protocol permission_request_id so the engine echoes it back", ev.RequestID)
	}
	if ev.ToolName != "shell" {
		t.Errorf("ToolName = %q, want shell", ev.ToolName)
	}
	if ev.ToolInputRaw["cmd"] != "go test ./..." {
		t.Errorf("ToolInputRaw lost input: %+v", ev.ToolInputRaw)
	}
}

func TestMapEvent_Cancelled(t *testing.T) {
	_, emit, terminal := mapEvent(protocolEvent{Type: "cancelled", Reason: "user_cancelled"})
	if !emit || !terminal {
		t.Fatalf("cancelled must be emitted and terminal, got emit=%v terminal=%v", emit, terminal)
	}
}

func TestMapEvent_UnknownTypeSkipped(t *testing.T) {
	_, emit, terminal := mapEvent(protocolEvent{Type: "some_future_event"})
	if emit || terminal {
		t.Errorf("unknown event type should be skipped, got emit=%v terminal=%v", emit, terminal)
	}
}

func TestAttachmentRefs_FromImagesAndFiles(t *testing.T) {
	imgs := []core.ImageAttachment{{MimeType: "image/png", Data: []byte("1234"), FileName: "a.png"}}
	files := []core.FileAttachment{{MimeType: "text/plain", Data: []byte("hello"), FileName: "b.txt"}}

	imgRefs := attachmentRefsFromImages(imgs)
	if len(imgRefs) != 1 || imgRefs[0].Kind != "image" || imgRefs[0].Name != "a.png" {
		t.Fatalf("image ref shape wrong: %+v", imgRefs)
	}
	if imgRefs[0].SizeBytes != 4 || imgRefs[0].MimeType != "image/png" {
		t.Errorf("image ref metadata wrong: %+v", imgRefs[0])
	}
	fileRefs := attachmentRefsFromFiles(files)
	if len(fileRefs) != 1 || fileRefs[0].Kind != "file" || fileRefs[0].Name != "b.txt" {
		t.Fatalf("file ref shape wrong: %+v", fileRefs)
	}
}

func TestMapPermissionResult(t *testing.T) {
	behavior, updated, msg := mapPermissionResult(core.PermissionResult{
		Behavior:     "deny",
		UpdatedInput: map[string]any{"x": 1},
		Message:      "not allowed",
	})
	if behavior != "deny" || msg != "not allowed" || updated["x"] != 1 {
		t.Errorf("mapPermissionResult lost fields: %q %v %q", behavior, updated, msg)
	}
}

func TestToAgentSessionInfo(t *testing.T) {
	got := toAgentSessionInfo(routeSessionInfo{
		ID:           "route_sess_1",
		Summary:      "Fix tests",
		MessageCount: 12,
		ModifiedAt:   "2026-05-20T09:55:00Z",
	})
	if got.ID != "route_sess_1" || got.Summary != "Fix tests" || got.MessageCount != 12 {
		t.Errorf("toAgentSessionInfo lost fields: %+v", got)
	}
	want, _ := time.Parse(time.RFC3339, "2026-05-20T09:55:00Z")
	if !got.ModifiedAt.Equal(want) {
		t.Errorf("ModifiedAt = %v, want %v", got.ModifiedAt, want)
	}
}
