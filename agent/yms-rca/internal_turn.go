package ymsagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// defaultAutoRestoreTimeout caps how long a hidden /connect <profile> turn
// may take before runInternalPrompt gives up. 30s is generous enough for
// MCP attach + token negotiation but tight enough that a stuck subprocess
// surfaces as a user-visible error rather than a silent hang.
const defaultAutoRestoreTimeout = 30 * time.Second

// runInternalPrompt drives a hidden yms-rca turn — e.g. an auto-restore
// `/connect <profile>` — without surfacing its events to the user.
//
// Lifecycle:
//
//  1. Set internalActive=true and install done + result channels.
//  2. Reset all turn-level latches so the hidden turn starts clean.
//  3. Write the hidden prompt frame with a `-restore` suffixed id so a
//     stale response/prompt ack from the prior turn can't influence state.
//  4. Wait for the "done" signal: EventResult (success), EventError
//     (failure), permission request (auto-deny + failure), ctx cancel,
//     or wait-timeout.
//  5. Then DRAIN: wait for EventResult, which only fires when the
//     subprocess has fully terminated the hidden prompt. This guarantees
//     trailing text / EventResult / env-switch events from a failed
//     /connect are routed to handleInternalEvent (and dropped) under
//     internalActive=true — they don't leak as user-visible output.
//     If drain exceeds drainTimeout, mark the session dead so the engine
//     recycles to a fresh subprocess for the next user message.
//  6. Clear internalActive and the channels.
//  7. Reset turn-level latches so the upcoming user turn is fresh.
//  8. On success, verify currentProfileName matches the requested profile.
//
// The caller must hold session.busy=true across this call AND the user
// prompt that follows — runInternalPrompt does not toggle busy.
func (s *session) runInternalPrompt(ctx context.Context, prompt, expectProfile string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultAutoRestoreTimeout
	}

	done := make(chan error, 1)
	result := make(chan struct{})
	s.internalMu.Lock()
	s.internalDone = done
	s.internalResult = result
	s.internalMu.Unlock()
	s.internalActive.Store(true)

	// Reset turn-level latches so handlers don't read stale state from
	// the prior turn.
	s.resetTurnLatches(prompt)

	id := fmt.Sprintf("cc-%d-restore", atomic.AddUint64(&s.seq, 1))
	s.currentPromptID.Store(id)

	frame := map[string]any{
		"type":    "prompt",
		"id":      id,
		"message": prompt,
	}

	if err := s.writeFrame(frame); err != nil {
		s.endInternalTurn()
		return fmt.Errorf("yms-rca: write hidden prompt: %w", err)
	}

	var resultErr error
	select {
	case resultErr = <-done:
	case <-ctx.Done():
		resultErr = ctx.Err()
	case <-time.After(timeout):
		resultErr = fmt.Errorf("yms-rca: auto-restore timeout after %s", timeout)
	}

	// Drain: wait for the subprocess's EventResult so trailing events from
	// a failed /connect (denial text, message_end customType=yms-command,
	// response command=prompt → maybeFinalizeSlashCommandTurn → EventResult,
	// possibly yms-rca.env-switch "local") are all suppressed via handle
	// InternalEvent. Without this, internalActive would flip false before
	// those events arrive and they would leak to s.events. The success path
	// is a no-op here — EventResult already signaled both done and result.
	//
	// ctx cancellation and drain timeout both fall through to "mark
	// session dead + cancel subprocess context" so any further events
	// from the hidden /connect get terminated at the source rather than
	// outliving the session.
	select {
	case <-result:
	case <-ctx.Done():
		slog.Warn("yms-rca: hidden turn drain aborted (ctx cancelled); marking session dead",
			"hidden_id", id, "ctx_err", ctx.Err())
		s.alive.Store(false)
		s.cancel()
	case <-time.After(timeout):
		slog.Warn("yms-rca: hidden turn drain timeout; marking session dead to force recycle",
			"hidden_id", id, "drain_timeout", timeout)
		s.alive.Store(false)
		s.cancel()
	}

	s.endInternalTurn()

	if resultErr != nil {
		return resultErr
	}

	// Verify the hidden /connect actually switched the profile. yms-rca
	// emits an EventResult even when the connection failed silently
	// (e.g. profile not found), so we double-check via the env-switch
	// side effect.
	if got := s.currentProfileName(); got != expectProfile {
		return fmt.Errorf("yms-rca: auto-restore did not switch to %q (current: %q)", expectProfile, got)
	}
	return nil
}

// endInternalTurn flips the active flag off, clears the channels, and
// resets latches so the upcoming user turn starts fresh.
func (s *session) endInternalTurn() {
	s.internalActive.Store(false)
	s.internalMu.Lock()
	s.internalDone = nil
	s.internalResult = nil
	s.internalMu.Unlock()
	// Caller will write the user prompt next; reset latches now so the
	// stale state from the hidden turn doesn't leak into it. The Send
	// path also resets, but that happens BEFORE Send decides to run a
	// hidden turn — so we need to reset here, between turns.
	s.resetTurnLatches("")
}

// signalInternalDone delivers a hidden-turn result. Safe to call from any
// goroutine; only the first signal per turn is delivered.
func (s *session) signalInternalDone(err error) {
	s.internalMu.Lock()
	ch := s.internalDone
	s.internalMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
		// already signalled — first wins
	}
}

// signalInternalResult marks the subprocess as having fully terminated
// the hidden prompt. Safe to call multiple times; only the first close
// has effect.
func (s *session) signalInternalResult() {
	s.internalMu.Lock()
	ch := s.internalResult
	// Nil out so a re-entrant emit can't double-close.
	if ch != nil {
		s.internalResult = nil
	}
	s.internalMu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// handleInternalEvent decides what to do with an event observed during
// an active hidden turn. EventText / EventThinking / EventToolUse /
// EventToolResult are dropped; EventError / EventResult / EventPermission
// Request terminate the wait for the hidden turn's caller — but
// internalActive STAYS TRUE until EventResult arrives (the subprocess's
// real terminal for the hidden prompt). This guarantees trailing events
// from a failing /connect don't leak as user-visible output.
//
// EventPermissionRequest is special: auto-restore cannot prompt the user,
// so we must (a) write extension_ui_response confirmed:false back to the
// yms-rca subprocess so it unblocks, and (b) signal the caller with an
// error. The subprocess's subsequent text/end events stay suppressed
// until the drain in runInternalPrompt sees EventResult.
func (s *session) handleInternalEvent(evt core.Event) {
	switch evt.Type {
	case core.EventResult:
		// EventResult is the subprocess's terminal event for the hidden
		// prompt. Signal both: success path uses done, failure path is
		// already done but still needs result to complete drain.
		s.signalInternalDone(nil)
		s.signalInternalResult()
	case core.EventError:
		err := evt.Error
		if err == nil {
			err = errors.New("yms-rca: auto-restore failed (no detail)")
		}
		s.signalInternalDone(err)
		// Don't signal result — subprocess may still emit further events
		// up to and including its own EventResult. Drain waits for that.
	case core.EventPermissionRequest:
		if evt.RequestID != "" {
			// nil emit — we don't want any user-visible reason text leaking
			// during a hidden turn.
			s.resolvePendingConfirm(evt.RequestID, false, "", nil)
		}
		s.signalInternalDone(errors.New("yms-rca: auto-restore cannot prompt user for permission"))
		// Don't signal result; subprocess will continue processing the
		// denial and eventually emit EventResult. Drain waits for that.
	default:
		// EventText, EventThinking, EventToolUse, EventToolResult — dropped.
		slog.Debug("yms-rca: suppressing event during hidden turn", "type", evt.Type)
	}
}

// resetTurnLatches resets all turn-level state that emit handlers consult.
// Called at the start of each turn (user or hidden). When prompt is "" the
// slash-command latch defaults to false.
func (s *session) resetTurnLatches(prompt string) {
	s.turnResultEmitted.Store(false)
	s.promptAcked.Store(false)
	s.slashCommandEnded.Store(false)
	s.assistantMessageEnded.Store(false)
	s.awaitingPostToolSummary.Store(false)
	s.postToolTextEmitted.Store(false)
	s.currentPromptSlashCommand.Store(prompt != "" && isSlashCommandPrompt(prompt))
	s.turnTextEmitted.Store(false)
	s.resetAssistantText()
	s.clearPendingTurnResult()
}

// maybeRestoreProfileBeforePrompt checks the persisted profile for this
// (project, session_key) and — if non-local — runs a hidden `/connect
// <profile>` so the user's first message after a daemon restart lands on
// the right MCP profile.
//
// Bypasses (no restore attempted, no error returned):
//
//   - user prompt is itself a slash command — /connect, /disconnect,
//     /status, /help etc.: the user's explicit intent wins; we don't
//     want to insert an MCP attach/detach cycle in front of it. The
//     restoreAttempted latch is NOT consumed here, so a later business
//     prompt in the same session still gets its restore chance.
//   - no project / session_key (programmatic test path; no relay).
//   - no profileStore wired.
//   - store has no entry, or entry is "local".
//   - already attempted (or pre-flight-failed) once this session
//     (restoreAttempted latch).
//
// If the stored profile name fails character-set validation, we clear
// the entry and skip — depth-in-defense against hand-edited store files.
//
// Note: we deliberately do NOT skip when `s.currentProfileName()` is
// non-local. That field is seeded from the agent-level last-known
// profile for footer display, but the freshly spawned subprocess always
// starts in "local" — so the inherited string says nothing about
// subprocess connection state, and trusting it would let one session's
// /connect mask another session's missing restore.
//
// On success returns nil and the session's currentProfileName matches the
// stored profile. On failure returns a wrapped error; the caller is
// expected to surface it to the user and NOT send the original prompt.
func (s *session) maybeRestoreProfileBeforePrompt(ctx context.Context, prompt string) error {
	// User's slash command always wins, and must NOT consume the one-
	// shot restoreAttempted latch — otherwise a /status as the first
	// message after a daemon restart would silently burn the only
	// auto-restore opportunity for the session. ParseConnectTarget is
	// a stricter subset of isSlashCommandPrompt and is checked
	// implicitly here.
	if isSlashCommandPrompt(prompt) {
		return nil
	}
	if s.profileStore == nil || s.project == "" || s.sessionKey == "" {
		return nil
	}
	profile := s.profileStore.Get(s.project, s.sessionKey)
	if profile == "" || profile == "local" {
		return nil
	}
	if !isValidProfileName(profile) {
		slog.Warn("yms-rca: stored profile name invalid, clearing entry",
			"project", s.project, "profile", profile)
		s.profileStore.Clear(s.project, s.sessionKey)
		return nil
	}
	// Latch BEFORE attempting (including pre-flight) so a failed restore
	// isn't retried on every subsequent prompt — once the user has seen
	// the error, they should /connect manually rather than have us spin
	// the same failure.
	if !s.restoreAttempted.CompareAndSwap(false, true) {
		return nil
	}
	// Pre-flight env-var check so a stale token in the store surfaces
	// a clear error without spending a yms-rca turn.
	if s.cfg != nil {
		if err := s.cfg.validateConnectionTokenEnv(profile); err != nil {
			return fmt.Errorf("yms-rca: auto-restore profile %q failed: %w", profile, err)
		}
	}
	if err := s.runInternalPrompt(ctx, "/connect "+profile, profile, defaultAutoRestoreTimeout); err != nil {
		// Plain English error matches the convention of other yms-rca
		// (and all other agent) errors in this repo; engine wraps with
		// the i18n MsgError prefix ("❌ 错误: %s" / "❌ Error: %s") when
		// delivering to the platform, so the localised part is the
		// prefix while the agent-detail stays consistent.
		return fmt.Errorf("yms-rca: auto-restore profile %q failed: %w; please re-run /connect %s", profile, err, profile)
	}
	return nil
}

var _ = sync.Once{} // retained for future refactors
