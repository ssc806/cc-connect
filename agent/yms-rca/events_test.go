package ymsagent

import (
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func drainEvents(t *testing.T, s *session, d time.Duration) []core.Event {
	t.Helper()
	deadline := time.Now().Add(d)
	var out []core.Event
	for time.Now().Before(deadline) {
		select {
		case e, ok := <-s.events:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return out
}

func TestHandleEvent_TextDelta(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "hello",
		},
	})
	events := drainEvents(t, s, 100*time.Millisecond)
	if len(events) != 1 || events[0].Type != core.EventText || events[0].Content != "hello" {
		t.Fatalf("got: %+v", events)
	}
}

func TestHandleEvent_ThinkingFlushOnEnd(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "abc"},
	})
	// no event yet
	events := drainEvents(t, s, 50*time.Millisecond)
	if len(events) != 0 {
		t.Fatalf("thinking_delta should not emit immediately; got %+v", events)
	}
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_end"},
	})
	events = drainEvents(t, s, 100*time.Millisecond)
	if len(events) != 1 || events[0].Type != core.EventThinking || events[0].Content != "abc" {
		t.Fatalf("got: %+v", events)
	}
}

func TestHandleEvent_GetStateLearnsSessionID(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "response", "command": "get_state", "success": true,
		"data": map[string]any{"sessionId": "abc-123"},
	})
	if got := s.CurrentSessionID(); got != "abc-123" {
		t.Errorf("session id not learned, got %q", got)
	}
}

func TestHandleEvent_GetSessionStats(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "response", "command": "get_session_stats", "success": true,
		"data": map[string]any{
			"contextUsage": map[string]any{"tokens": 1234.0, "contextWindow": 200000.0, "percent": 0.6},
			"tokens":       map[string]any{"total": 5000.0, "input": 3000.0, "output": 2000.0, "cacheRead": 100.0},
		},
	})
	usage := s.GetContextUsage()
	if usage == nil {
		t.Fatal("usage was nil")
	}
	if usage.UsedTokens != 1234 || usage.ContextWindow != 200000 {
		t.Errorf("contextUsage map wrong: %+v", usage)
	}
	if usage.InputTokens != 3000 || usage.OutputTokens != 2000 || usage.CachedInputTokens != 100 || usage.TotalTokens != 5000 {
		t.Errorf("tokens map wrong: %+v", usage)
	}
}

func TestHandleEvent_PromptFailure(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.busy.Store(true)
	s.handleEvent(map[string]any{
		"type": "response", "command": "prompt", "success": false,
		"data": map[string]any{"error": "boom"},
	})
	events := drainEvents(t, s, 100*time.Millisecond)
	if len(events) != 1 || events[0].Type != core.EventError {
		t.Fatalf("got: %+v", events)
	}
	if s.busy.Load() {
		t.Errorf("busy not cleared after prompt failure")
	}
}

func TestHandleEvent_AgentEnd(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.sessionID.Store("sid-1")
	s.busy.Store(true)
	s.handleEvent(map[string]any{
		"type":  "agent_end",
		"usage": map[string]any{"input": 100.0, "output": 50.0},
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	var got *core.Event
	for i := range evts {
		if evts[i].Type == core.EventResult {
			got = &evts[i]
		}
	}
	if got == nil {
		t.Fatalf("no EventResult; got=%+v", evts)
	}
	if got.SessionID != "sid-1" || got.InputTokens != 100 || got.OutputTokens != 50 {
		t.Errorf("unexpected: %+v", got)
	}
	if s.busy.Load() {
		t.Errorf("busy not cleared")
	}
}

func TestHandleEvent_ToolcallEndWithoutStartEmits(t *testing.T) {
	// When tool_execution_start is NOT seen first, toolcall_end should
	// still emit EventToolUse via the message_update path (covers
	// extractToolInput / emitToolFromMessage end-to-end).
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": 0.0,
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":      "toolCall",
						"id":        "tc-direct",
						"name":      "grep",
						"arguments": map[string]any{"pattern": "TODO"},
					},
				},
			},
		},
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventToolUse {
		t.Fatalf("got %+v", evts)
	}
	if evts[0].ToolName != "grep" || evts[0].ToolInput != "TODO" {
		t.Errorf("event payload wrong: %+v", evts[0])
	}
}

func TestHandleEvent_ToolExecutionStartEnd_Dedup(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "tool_execution_start", "toolCallId": "tc-1", "toolName": "bash",
		"arguments": map[string]any{"command": "ls -la"},
	})
	// message_update / toolcall_end carrying same toolCallId should be skipped.
	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": 0.0,
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":      "toolCall",
						"id":        "tc-1",
						"name":      "bash",
						"arguments": map[string]any{"command": "ls -la"},
					},
				},
			},
		},
	})
	// tool_execution_end
	s.handleEvent(map[string]any{
		"type": "tool_execution_end", "toolCallId": "tc-1", "toolName": "bash",
		"result": "files", "success": true,
	})
	// message_end role=toolResult with same id should be skipped.
	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "toolResult",
			"toolName":   "bash",
			"toolCallId": "tc-1",
			"content":    []any{map[string]any{"text": "files"}},
		},
	})

	evts := drainEvents(t, s, 100*time.Millisecond)
	var uses, results int
	for _, e := range evts {
		if e.Type == core.EventToolUse {
			uses++
		}
		if e.Type == core.EventToolResult {
			results++
		}
	}
	if uses != 1 {
		t.Errorf("expected 1 EventToolUse (dedup), got %d", uses)
	}
	if results != 1 {
		t.Errorf("expected 1 EventToolResult (dedup), got %d", results)
	}
}

func TestHandleEvent_CustomYmsCommandStrips(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "custom",
			"display":    true,
			"customType": "yms-command",
			"content":    []any{map[string]any{"text": "\x1b[31mred\x1b[0m\n\n\n\n"}},
		},
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventText {
		t.Fatalf("got %+v", evts)
	}
	if strings.Contains(evts[0].Content, "\x1b") {
		t.Errorf("ANSI not stripped: %q", evts[0].Content)
	}
}

func TestHandleEvent_NotifyError(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "extension_ui_request", "id": "n1", "method": "notify",
		"notifyType": "error", "message": "oops",
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventError {
		t.Fatalf("got %+v", evts)
	}
}

func TestHandleEvent_SelectCancels(t *testing.T) {
	s, enc := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "extension_ui_request", "id": "s1", "method": "select",
	})
	frames := enc.framesCopy()
	if len(frames) != 1 || frames[0]["cancelled"] != true || frames[0]["id"] != "s1" {
		t.Fatalf("expected cancelled frame; got %+v", frames)
	}
}

func TestHandleEvent_NonResponseAssistantError(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":         "assistant",
			"errorMessage": "boom",
		},
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventError {
		t.Fatalf("got %+v", evts)
	}
}

func TestHandleEvent_ToolExecutionUpdate_ProgressAsThinking(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type":       "tool_execution_update",
		"toolCallId": "tc-u",
		"progress":   "step 2/5",
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventThinking || evts[0].Content != "step 2/5" {
		t.Fatalf("got %+v", evts)
	}
}

func TestHandleEvent_ToolExecutionUpdate_EmptyDropped(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type":       "tool_execution_update",
		"toolCallId": "tc-empty",
	})
	evts := drainEvents(t, s, 50*time.Millisecond)
	if len(evts) != 0 {
		t.Fatalf("empty progress should be dropped; got %+v", evts)
	}
}

func TestHandleEvent_ExtensionError(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type":    "extension_error",
		"message": "extension blew up",
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventError {
		t.Fatalf("got %+v", evts)
	}
	if evts[0].Error == nil || !strings.Contains(evts[0].Error.Error(), "extension blew up") {
		t.Errorf("error text wrong: %v", evts[0].Error)
	}
}

func TestHandleEvent_ContextUsage_NilSafe_KeepsPrevious(t *testing.T) {
	s, _ := newTestSession(t, "default")
	// Prime with a real value.
	s.handleEvent(map[string]any{
		"type": "response", "command": "get_session_stats", "success": true,
		"data": map[string]any{
			"contextUsage": map[string]any{"tokens": 1000.0, "contextWindow": 200000.0},
		},
	})
	first := s.GetContextUsage()
	if first == nil || first.UsedTokens != 1000 {
		t.Fatalf("priming failed: %+v", first)
	}

	// Second event with tokens=nil (compaction in progress) — must NOT clobber.
	s.handleEvent(map[string]any{
		"type": "response", "command": "get_session_stats", "success": true,
		"data": map[string]any{
			"contextUsage": map[string]any{"tokens": nil, "contextWindow": 200000.0},
		},
	})
	second := s.GetContextUsage()
	if second == nil || second.UsedTokens != 1000 {
		t.Errorf("compaction nil clobbered previous usage; got %+v", second)
	}
}

func TestHandleEvent_CustomEnvSwitch_DisplayTrueEmitsText(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "custom",
			"display":    true,
			"customType": "yms-rca.env-switch",
			"content":    []any{map[string]any{"text": "switched to pre"}},
		},
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventText {
		t.Fatalf("display=true should emit EventText; got %+v", evts)
	}
}

func TestHandleEvent_CustomEnvSwitch_DisplayFalseEmitsThinking(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "custom",
			"display":    false,
			"customType": "yms-rca.env-switch",
			"content":    []any{map[string]any{"text": "internal"}},
		},
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventThinking {
		t.Fatalf("display=false should emit EventThinking; got %+v", evts)
	}
}

func TestHandleEvent_AgentEnd_NoUsage(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.sessionID.Store("sid-no-usage")
	s.busy.Store(true)
	s.handleEvent(map[string]any{"type": "agent_end"})
	evts := drainEvents(t, s, 100*time.Millisecond)
	var ev *core.Event
	for i := range evts {
		if evts[i].Type == core.EventResult {
			ev = &evts[i]
		}
	}
	if ev == nil {
		t.Fatalf("missing EventResult: %+v", evts)
	}
	if ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.SessionID != "sid-no-usage" {
		t.Errorf("unexpected: %+v", ev)
	}
	if s.busy.Load() {
		t.Error("busy not cleared")
	}
}

func TestHandleEvent_UnknownTypeIgnored(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{"type": "totally_made_up"})
	evts := drainEvents(t, s, 50*time.Millisecond)
	if len(evts) != 0 {
		t.Fatalf("unknown type should not emit; got %+v", evts)
	}
}

func TestHandleEvent_ExtensionUI_NotifyPlain(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.handleEvent(map[string]any{
		"type": "extension_ui_request", "id": "n2", "method": "notify",
		"message": "fyi",
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventThinking || evts[0].Content != "fyi" {
		t.Fatalf("got %+v", evts)
	}
}

func TestHandleEvent_PromptFailureUsesErrorMessage(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.busy.Store(true)
	s.handleEvent(map[string]any{
		"type": "response", "command": "prompt", "success": false,
		"errorMessage": "rate limit",
	})
	evts := drainEvents(t, s, 100*time.Millisecond)
	if len(evts) != 1 || evts[0].Type != core.EventError {
		t.Fatalf("got %+v", evts)
	}
	if !strings.Contains(evts[0].Error.Error(), "rate limit") {
		t.Errorf("error text: %v", evts[0].Error)
	}
}
