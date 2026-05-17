package ymsagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// runInternalPromptAsync calls runInternalPrompt in a goroutine and returns
// a channel for the eventual error. Used to drive event injection in tests.
func runInternalPromptAsync(t *testing.T, s *session, prompt, profile string, timeout time.Duration) <-chan error {
	t.Helper()
	out := make(chan error, 1)
	go func() {
		out <- s.runInternalPrompt(context.Background(), prompt, profile, timeout)
	}()
	// Give the goroutine a tick to flip internalActive and write the frame.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.internalActive.Load() {
			return out
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("internalActive did not become true within 200ms")
	return out
}

func TestRunInternalPromptSuppressesText(t *testing.T) {
	s, enc := newTestSession(t, "default")

	done := runInternalPromptAsync(t, s, "/connect pre", "pre", 2*time.Second)

	// Simulate yms-rca turn: text, profile env-switch, then final Result.
	s.emit(core.Event{Type: core.EventText, Content: "Connected to pre"})
	s.emit(core.Event{Type: core.EventText, Content: "*profile: pre*"})
	// Profile must update so verification at end of runInternalPrompt passes.
	s.updateCurrentProfile("pre")
	s.emit(core.Event{Type: core.EventResult, Done: true})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInternalPrompt = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runInternalPrompt did not return")
	}

	// No user-visible events should have been emitted.
	events := drainEvents(t, s, 50*time.Millisecond)
	if len(events) != 0 {
		t.Errorf("hidden turn leaked %d events: %+v", len(events), events)
	}

	// Hidden prompt frame should be present with the -restore id.
	frames := enc.framesCopy()
	if len(frames) != 1 {
		t.Fatalf("want 1 frame written, got %d", len(frames))
	}
	if got := frames[0]["message"]; got != "/connect pre" {
		t.Errorf("frame message = %v, want /connect pre", got)
	}
	id, _ := frames[0]["id"].(string)
	if id == "" || !contains(id, "-restore") {
		t.Errorf("expected hidden prompt id to contain '-restore', got %q", id)
	}

	// internalActive should be cleared after return.
	if s.internalActive.Load() {
		t.Error("internalActive still true after return")
	}
}

func TestRunInternalPromptCapturesError(t *testing.T) {
	s, _ := newTestSession(t, "default")

	done := runInternalPromptAsync(t, s, "/connect pre", "pre", 2*time.Second)

	wantErr := errors.New(`connection "pre" needs env IUAPYYS_MCP_TOKEN`)
	s.emit(core.Event{Type: core.EventError, Error: wantErr})
	// Subprocess follow-up: after the error, yms-rca still emits its
	// terminal EventResult. The drain in runInternalPrompt waits for it
	// before flipping internalActive=false (Finding 1 fix from PR #10
	// review round 2).
	s.emit(core.Event{Type: core.EventResult, Done: true})

	select {
	case err := <-done:
		if err == nil || !contains(err.Error(), "IUAPYYS_MCP_TOKEN") {
			t.Fatalf("runInternalPrompt err = %v, want IUAPYYS_MCP_TOKEN", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runInternalPrompt did not return")
	}
}

func TestRunInternalPromptTimesOut(t *testing.T) {
	s, _ := newTestSession(t, "default")

	done := runInternalPromptAsync(t, s, "/connect pre", "pre", 50*time.Millisecond)
	// Don't emit anything; let both the wait timeout and the drain
	// timeout fire. Drain timeout marks the session dead — that's the
	// new contract for "subprocess hung mid hidden turn".

	select {
	case err := <-done:
		if err == nil || !contains(err.Error(), "timeout") {
			t.Fatalf("runInternalPrompt err = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runInternalPrompt did not time out")
	}
	if s.alive.Load() {
		t.Error("session should be marked dead after drain timeout")
	}
}

func TestRunInternalPromptAutoDeniesPermissionRequest(t *testing.T) {
	s, enc := newTestSession(t, "default")
	// Register a pending so resolvePendingConfirm has something to ack.
	s.registerPending("req-7", "MCP attach approval?")

	done := runInternalPromptAsync(t, s, "/connect pre", "pre", time.Second)

	s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: "req-7", ToolName: "approve"})
	// Subprocess: after receiving confirmed:false it processes the denial
	// and emits its terminal EventResult; drain waits for this.
	s.emit(core.Event{Type: core.EventResult, Done: true})

	select {
	case err := <-done:
		if err == nil || !contains(err.Error(), "permission") {
			t.Fatalf("runInternalPrompt err = %v, want permission-related error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runInternalPrompt did not return after permission request")
	}

	// Confirm an extension_ui_response was written with confirmed:false so
	// yms-rca subprocess doesn't hang.
	frames := enc.framesCopy()
	found := false
	for _, f := range frames {
		if f["type"] == "extension_ui_response" && f["id"] == "req-7" {
			if confirmed, ok := f["confirmed"].(bool); ok && !confirmed {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected extension_ui_response confirmed:false frame, got: %+v", frames)
	}
}

func TestRunInternalPromptResetsLatchesAfterCompletion(t *testing.T) {
	s, _ := newTestSession(t, "default")
	// Pre-set some latches to make sure they get reset.
	s.turnResultEmitted.Store(true)
	s.promptAcked.Store(true)
	s.assistantMessageEnded.Store(true)

	done := runInternalPromptAsync(t, s, "/connect pre", "pre", time.Second)
	s.updateCurrentProfile("pre")
	s.emit(core.Event{Type: core.EventResult, Done: true})

	<-done

	// After hidden turn, latches should be cleared so the upcoming user
	// turn starts fresh.
	if s.turnResultEmitted.Load() {
		t.Error("turnResultEmitted not reset after hidden turn")
	}
	if s.promptAcked.Load() {
		t.Error("promptAcked not reset after hidden turn")
	}
	if s.assistantMessageEnded.Load() {
		t.Error("assistantMessageEnded not reset after hidden turn")
	}
}

func TestRunInternalPromptVerifiesProfileApplied(t *testing.T) {
	s, _ := newTestSession(t, "default")

	done := runInternalPromptAsync(t, s, "/connect pre", "pre", time.Second)
	// Result fires WITHOUT updateCurrentProfile, so the verify-after-success
	// check must fail.
	s.emit(core.Event{Type: core.EventResult, Done: true})

	select {
	case err := <-done:
		if err == nil || !contains(err.Error(), "did not switch") {
			t.Fatalf("expected verify-failed error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runInternalPrompt did not return")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// ── maybeRestoreProfileBeforePrompt ────────────────────────────────

func TestMaybeRestoreSkipsWhenNoPersistedProfile(t *testing.T) {
	s, _ := newTestSessionWithStore(t, "p", "k")
	// store empty
	if err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗"); err != nil {
		t.Errorf("with empty store want nil, got %v", err)
	}
	// second call returns nil (restoreAttempted latched)
	if err := s.maybeRestoreProfileBeforePrompt(context.Background(), "again"); err != nil {
		t.Errorf("second call want nil, got %v", err)
	}
}

func TestMaybeRestoreSkipsWhenSlashCommand(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	for _, prompt := range []string{"/connect dev", "/disconnect", "/status", "  /help  "} {
		s.restoreAttempted.Store(false) // reset between cases
		err := s.maybeRestoreProfileBeforePrompt(context.Background(), prompt)
		if err != nil {
			t.Errorf("slash command %q should bypass restore, got err %v", prompt, err)
		}
		// no hidden frame should have been written
		if s.internalActive.Load() {
			t.Errorf("internalActive set for slash command %q", prompt)
		}
	}
}

// TestSlashCommandBypassDoesNotConsumeRestoreLatch verifies the
// PR #10 review-round-2 fix for Finding 2a: when the first message
// after a daemon restart is a slash command (e.g. /status), the
// one-shot restoreAttempted latch must NOT be set, so a subsequent
// business prompt still triggers the auto-restore.
func TestSlashCommandBypassDoesNotConsumeRestoreLatch(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	// First message: /status. Should bypass cleanly without latching.
	if err := s.maybeRestoreProfileBeforePrompt(context.Background(), "/status"); err != nil {
		t.Fatalf("slash bypass returned err: %v", err)
	}
	if s.restoreAttempted.Load() {
		t.Fatal("restoreAttempted latched by /status — would silently burn the only restore chance")
	}

	// Second message: business prompt — must still trigger restore.
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.updateCurrentProfile("pre")
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	if err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗"); err != nil {
		t.Fatalf("subsequent business prompt should restore, got err: %v", err)
	}
	if !s.restoreAttempted.Load() {
		t.Error("restoreAttempted should be true after actual attempt")
	}
}

// TestMaybeRestoreProceedsEvenIfInheritedProfileIsNonLocal verifies that
// auto-restore does NOT short-circuit on the session's `currentProfile`
// field being non-local. That field is seeded from the agent-level
// last-known profile (for footer display) — it does NOT reflect the
// freshly-spawned subprocess's MCP connection state, which always starts
// in "local". Relying on it would let one session's prior /connect mask
// another session's missing restore.
func TestMaybeRestoreProceedsEvenIfInheritedProfileIsNonLocal(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")
	s.currentProfile.Store("pre") // inherited from agent — not subprocess truth

	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.updateCurrentProfile("pre")
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	if err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗"); err != nil {
		t.Errorf("restore should proceed despite non-local inherited profile, got %v", err)
	}
}

func TestMaybeRestoreRunsOnceOnly(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")
	s.restoreAttempted.Store(true) // simulate "already attempted"

	if err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗"); err != nil {
		t.Errorf("second-time call should be no-op, got %v", err)
	}
}

func TestMaybeRestoreInvokesHiddenConnect(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	// Drive the hidden turn from another goroutine: when internalActive
	// flips on, simulate yms-rca completing the /connect successfully.
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.updateCurrentProfile("pre")
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗")
	if err != nil {
		t.Fatalf("maybeRestore returned %v, want nil", err)
	}
	if got := s.currentProfileName(); got != "pre" {
		t.Errorf("after restore currentProfileName = %q, want pre", got)
	}
}

// TestMaybeRestoreFailureErrorShape verifies the error returned to the
// engine carries enough context for the user to act on it: profile name,
// underlying cause, and a "/connect <profile>" recovery hint. Text is
// plain English — matching the convention of all other agent errors in
// this repo; the engine's MsgError template handles language-of-prefix.
func TestMaybeRestoreFailureErrorShape(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.emit(core.Event{Type: core.EventError, Error: errors.New("token missing")})
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"yms-rca: auto-restore profile",
		`"pre"`,
		"failed",
		"token missing",         // underlying cause wrapped via %w
		"please re-run /connect pre",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got %q", want, msg)
		}
	}
}

func TestMaybeRestoreRejectsInvalidStoredProfile(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	// Bypass the Set guard by writing directly to the in-memory map. This
	// simulates a hand-edited store file or a legacy entry surviving load.
	store.mu.Lock()
	store.data.Projects = map[string]map[string]profileEntry{
		"p": {"k": {Profile: "bad name", UpdatedAt: "now"}},
	}
	store.mu.Unlock()

	err := s.maybeRestoreProfileBeforePrompt(context.Background(), "流量切入了吗")
	if err != nil {
		t.Errorf("invalid profile should be silently skipped (warn + clear), got %v", err)
	}
	// The invalid entry should now be cleared from the store.
	if got := store.Get("p", "k"); got != "" {
		t.Errorf("invalid entry should be cleared, store has %q", got)
	}
}

// ── Noise-event suppression during hidden turn ────────────────────────

// emitAndCheckDropped emits an event during an active internal turn and
// asserts that the user-facing events channel sees nothing. The hidden
// turn is left open so the test driver can keep emitting; cleanup ends it.
func TestRunInternalPromptSuppressesNoiseEventTypes(t *testing.T) {
	cases := []core.Event{
		{Type: core.EventText, Content: "Connected to pre"},
		{Type: core.EventThinking, Content: "thinking..."},
		{Type: core.EventToolUse, ToolName: "mcp_attach", ToolInput: "{}"},
		{Type: core.EventToolResult, ToolName: "mcp_attach", ToolResult: "ok"},
	}
	for _, evt := range cases {
		t.Run(string(evt.Type), func(t *testing.T) {
			s, _ := newTestSession(t, "default")

			done := runInternalPromptAsync(t, s, "/connect pre", "pre", 2*time.Second)

			s.emit(evt)

			// Confirm nothing leaked to user channel.
			leaked := drainEvents(t, s, 30*time.Millisecond)
			if len(leaked) != 0 {
				t.Errorf("event type %s leaked: %+v", evt.Type, leaked)
			}

			// End the hidden turn cleanly so the goroutine returns.
			s.updateCurrentProfile("pre")
			s.emit(core.Event{Type: core.EventResult, Done: true})
			<-done
		})
	}
}

// ── Send() integration: busy releasing on restore failure ────────────

// TestSendReleasesBusyOnRestoreFailure pins the highest-impact contract:
// when auto-restore fails inside Send(), the busy flag MUST be cleared,
// otherwise every subsequent Send would be rejected with "previous turn
// still running" and the session hangs until subprocess restart.
func TestSendReleasesBusyOnRestoreFailure(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	// Drive an immediate failure from the hidden turn.
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.emit(core.Event{Type: core.EventError, Error: errors.New("token missing")})
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	err := s.Send("流量切入了吗", nil, nil)
	if err == nil {
		t.Fatalf("Send should fail when restore fails, got nil")
	}
	if !strings.Contains(err.Error(), "yms-rca: auto-restore profile") {
		t.Errorf("error should describe auto-restore failure, got %q", err.Error())
	}

	// CRITICAL: busy must be released so the next Send can proceed.
	if s.busy.Load() {
		t.Fatal("busy still true after restore failure — would hang next Send")
	}

	// Also confirm: no user prompt frame was written to yms-rca. Only the
	// hidden /connect frame should have appeared.
	// (Note: writeFrame routes through mockEncoder in tests.)
	// We can't see frames from here (enc isn't returned by newTestSessionWithStore),
	// but the busy assertion above is the user-visible contract.
}

// TestSendAfterRestoreFailureCanRetry exercises the recovery flow that
// motivates the busy-release contract: user gets the failure message,
// fixes the env, and re-issues the prompt. The second Send must NOT be
// rejected.
func TestSendAfterRestoreFailureCanRetry(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.emit(core.Event{Type: core.EventError, Error: errors.New("first attempt fails")})
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	if err := s.Send("first prompt", nil, nil); err == nil {
		t.Fatalf("first Send should fail")
	}

	// restoreAttempted is now true, so the second Send bypasses restore
	// and writes the user prompt directly. busy must allow it through.
	if err := s.Send("second prompt after fixup", nil, nil); err != nil {
		t.Fatalf("second Send rejected: %v — busy was not released?", err)
	}
}

// ── Inherited profile must not skip restore ──────────────────────────

// TestSendRunsRestoreEvenWhenInheritedProfileIsNonLocal verifies that
// Send issues a hidden `/connect <profile>` on first call even when
// `currentProfile` was seeded non-local from the agent. The freshly
// spawned subprocess always starts in "local" — the inherited string is
// for footer display only and must not gate restore. (Regression test
// for the multi-session scenario where session A's /connect would have
// masked session B's missing restore.)
func TestSendRunsRestoreEvenWhenInheritedProfileIsNonLocal(t *testing.T) {
	s, store, _, enc := newSessionWithEncoder(t)
	store.Set("p", "k", "pre")
	s.profileStore = store
	s.project = "p"
	s.sessionKey = "k"
	// Simulate agent-inherited profile (footer signal, not subprocess truth).
	s.currentProfile.Store("pre")

	// Drive the hidden turn to success so Send returns cleanly.
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.updateCurrentProfile("pre")
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	if err := s.Send("普通问题", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	frames := enc.framesCopy()
	var sawRestore bool
	for _, f := range frames {
		if strings.Contains(asString(f, "id", ""), "-restore") {
			sawRestore = true
			break
		}
	}
	if !sawRestore {
		t.Errorf("expected hidden /connect -restore frame despite non-local inherited profile, got frames=%+v", frames)
	}
}

// newSessionWithEncoder is like newTestSessionWithStore but also returns
// the mockEncoder so callers can assert frames written.
func newSessionWithEncoder(t *testing.T) (*session, *profileStore, string, *mockEncoder) {
	t.Helper()
	s, enc := newTestSession(t, "default")
	dir := t.TempDir()
	path := dir + "/store.json"
	store := newProfileStore(path)
	return s, store, path, enc
}

// ── Hidden turn ctx cancellation ──────────────────────────────────────

func TestRunInternalPromptHonorsContextCancellation(t *testing.T) {
	s, _ := newTestSession(t, "default")
	ctx, cancel := context.WithCancel(context.Background())

	out := make(chan error, 1)
	go func() { out <- s.runInternalPrompt(ctx, "/connect pre", "pre", 5*time.Second) }()

	// Wait for internal turn to start.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && !s.internalActive.Load() {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-out:
		if err == nil {
			t.Fatal("want non-nil error from cancelled ctx")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runInternalPrompt did not return after ctx cancel")
	}
}

// ── Subprocess exit during hidden turn ───────────────────────────────

// TestSubprocessExitDuringHiddenTurnDoesNotLeakLifecycleEventResult is
// the regression test for PR #10 review round 4. readStdout's tail uses
// tryEmit() to push a synthetic lifecycle EventResult — which bypasses
// emit()'s internalActive routing. Without the fix that EventResult
// would land in s.events and the engine would treat it as a successful
// user-turn result (delivering an empty reply and hiding the underlying
// auto-restore failure). It would also leave runInternalPrompt's drain
// waiting on internalResult until the drain timeout.
func TestSubprocessExitDuringHiddenTurnDoesNotLeakLifecycleEventResult(t *testing.T) {
	s, _ := newTestSession(t, "default")

	// Simulate runInternalPrompt being mid-flight: hidden lifecycle
	// channels installed and internalActive=true.
	done := make(chan error, 1)
	result := make(chan struct{})
	s.internalMu.Lock()
	s.internalDone = done
	s.internalResult = result
	s.internalMu.Unlock()
	s.internalActive.Store(true)
	s.busy.Store(true)

	// Trigger readStdout's exit tail.
	s.finalizeSubprocessExit()

	// Hidden caller must be unblocked with an error (not nil), so
	// runInternalPrompt returns rather than hanging on drain.
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("hidden done signal must carry an error, got nil")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subprocess exit did not signal hidden done")
	}

	// Drain channel must be closed so runInternalPrompt's drain returns
	// immediately rather than waiting drain timeout.
	select {
	case <-result:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subprocess exit did not signal hidden result; drain would block")
	}

	// CRITICAL: no synthetic EventResult may have leaked to s.events.
	// Otherwise the engine treats the hidden turn as a successful user
	// turn and delivers an empty reply that masks the real failure.
	select {
	case evt := <-s.events:
		t.Errorf("lifecycle EventResult leaked to user channel during hidden turn: %+v", evt)
	default:
	}

	if s.busy.Load() {
		t.Error("busy should be cleared after subprocess exit")
	}

	// Session must be marked dead so the engine's post-Send cleanup
	// recycles it instead of re-using a session whose subprocess has
	// exited and whose stdin is closed.
	if s.Alive() {
		t.Error("session must be marked dead after subprocess exit during hidden turn — engine recycle depends on Alive()==false")
	}
}

// TestSubprocessExitOutsideHiddenTurnStillEmitsLifecycleEventResult
// guards the non-hidden path: the engine relies on the lifecycle Event
// Result to finalize a user turn that was in flight when the subprocess
// died, so we must keep emitting it when no hidden turn is active.
func TestSubprocessExitOutsideHiddenTurnStillEmitsLifecycleEventResult(t *testing.T) {
	s, _ := newTestSession(t, "default")
	s.busy.Store(true)
	// internalActive is false by default — no hidden turn.

	s.finalizeSubprocessExit()

	select {
	case evt := <-s.events:
		if evt.Type != core.EventResult {
			t.Errorf("expected EventResult, got %s", evt.Type)
		}
		if !evt.Done {
			t.Error("expected Done=true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("lifecycle EventResult was not emitted outside hidden turn")
	}

	if s.busy.Load() {
		t.Error("busy should be cleared")
	}
}

// ── Trailing-event suppression after hidden turn signals failure ─────

// TestHiddenTurnDrainSuppressesTrailingEvents verifies the PR #10
// review-round-2 fix for Finding 1: after the hidden turn's caller has
// been signalled with a failure (EventPermissionRequest, EventError),
// internalActive must stay true until the subprocess emits its real
// terminal EventResult — otherwise trailing text / EventResult / env-
// switch events from the failing /connect leak to s.events as user-
// visible output and may corrupt the persisted profile store.
func TestHiddenTurnDrainSuppressesTrailingEvents(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	// Drive the hidden turn: simulate yms-rca sending a permission
	// request first (failure signal), then — after a short delay — its
	// trailing events: a "Connection failed" text, an env-switch back
	// to local (which would erase the store if it reached the
	// authoritative path), and finally the subprocess terminal Event
	// Result.
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.internalActive.Load() {
				s.registerPending("req-restore", "MCP attach approval")
				s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: "req-restore", ToolName: "approve"})

				// Caller has now been signalled with an error. Simulate
				// the subprocess continuing to emit events for the
				// hidden prompt before its EventResult.
				time.Sleep(20 * time.Millisecond)
				s.emit(core.Event{Type: core.EventText, Content: "Connection failed: permission denied"})
				// Authoritative env-switch path — would clear the store
				// if internalActive had flipped false prematurely.
				emitEnvSwitch(s, "local")
				time.Sleep(10 * time.Millisecond)
				// Subprocess finally emits its terminal.
				s.emit(core.Event{Type: core.EventResult, Done: true})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	err := s.runInternalPrompt(context.Background(), "/connect pre", "pre", time.Second)
	if err == nil || !contains(err.Error(), "permission") {
		t.Fatalf("runInternalPrompt err = %v, want permission-related error", err)
	}

	// Trailing text must NOT have leaked to s.events.
	for {
		select {
		case evt := <-s.events:
			t.Errorf("trailing event leaked to user channel: %+v", evt)
		default:
			goto eventsChecked
		}
	}
eventsChecked:

	// Store must still hold "pre". The trailing env-switch to "local"
	// arrived during the drain window; updateCurrentProfile must skip
	// the store mutation under internalActive so a denial-path env-
	// switch can't erase the auto-restore target.
	if got := store.Get("p", "k"); got != "pre" {
		t.Errorf("store should still hold pre after denial-path env-switch during drain, got %q", got)
	}
}

// TestEnvSwitchDuringHiddenDrainDoesNotMutateStore is the focused
// regression test for Finding 3 from PR #10 review round 3: env-switch
// fires via handleMessageEnd (NOT via emit), so internalActive is the
// only signal that lets updateCurrentProfile distinguish a real user-
// driven /disconnect from a denial-path env-switch in a failing hidden
// /connect.
func TestEnvSwitchDuringHiddenDrainDoesNotMutateStore(t *testing.T) {
	t.Run("denial path env-switch to local must NOT clear store", func(t *testing.T) {
		s, store := newTestSessionWithStore(t, "p", "k")
		store.Set("p", "k", "pre")
		s.internalActive.Store(true)
		defer s.internalActive.Store(false)

		// Simulate yms-rca emitting yms-rca.env-switch to=local during
		// the drain of a failed /connect pre (e.g. permission denied).
		emitEnvSwitch(s, "local")

		if got := store.Get("p", "k"); got != "pre" {
			t.Errorf("store should hold pre after drain env-switch local, got %q", got)
		}
		// In-memory still reflects subprocess truth so the footer is
		// honest and runInternalPrompt's expectProfile check can fail.
		if got := s.currentProfileName(); got != "local" {
			t.Errorf("in-memory profile should reflect subprocess truth, got %q", got)
		}
	})

	t.Run("env-switch to other profile during drain must NOT persist", func(t *testing.T) {
		s, store := newTestSessionWithStore(t, "p", "k")
		store.Set("p", "k", "pre")
		s.internalActive.Store(true)
		defer s.internalActive.Store(false)

		// Subprocess emits env-switch to a different profile during
		// drain — store stays as "pre" (the auto-restore target).
		emitEnvSwitch(s, "dev")

		if got := store.Get("p", "k"); got != "pre" {
			t.Errorf("store should hold pre, got %q", got)
		}
	})

	t.Run("env-switch outside hidden turn still persists", func(t *testing.T) {
		// User-driven /connect dev — internalActive=false — must still
		// persist normally.
		s, store := newTestSessionWithStore(t, "p", "k")
		emitEnvSwitch(s, "dev")
		if got := store.Get("p", "k"); got != "dev" {
			t.Errorf("user env-switch should persist, got %q", got)
		}
	})

	t.Run("env-switch local outside hidden turn still clears store", func(t *testing.T) {
		// User-driven /disconnect — internalActive=false — must still
		// clear the store as before.
		s, store := newTestSessionWithStore(t, "p", "k")
		store.Set("p", "k", "pre")
		emitEnvSwitch(s, "local")
		if got := store.Get("p", "k"); got != "" {
			t.Errorf("user /disconnect should clear store, got %q", got)
		}
	})
}

func TestSignalInternalDoneDoesNotDeadlockOnDoubleSignal(t *testing.T) {
	s, _ := newTestSession(t, "default")
	done := make(chan error, 1)
	s.internalDone = done
	s.signalInternalDone(nil)
	// Second call must not block (select-default in implementation).
	doneCh := make(chan struct{})
	go func() {
		s.signalInternalDone(errors.New("ignored"))
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second signalInternalDone blocked")
	}
	// First signal wins.
	if err := <-done; err != nil {
		t.Errorf("first signal should win (nil), got %v", err)
	}
}
