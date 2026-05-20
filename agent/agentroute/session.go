package agentroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// closeRPCTimeout bounds each best-effort session.cancel / session.close RPC
// issued by Close(). It is deliberately small and independent of the
// configurable request timeout: Close() must hand control back to the engine
// quickly (the engine abandons AgentSession.Close() after 130s), and a
// cancel/close ack that does not arrive within a few seconds is not worth
// waiting for — the websocket teardown that follows ends the remote run anyway.
// It is a var, not a const, only so tests can shrink it.
var closeRPCTimeout = 5 * time.Second

// session implements core.AgentSession over one rpcClient connection.
//
// It owns the route session_id, tracks the active run_id and the
// permission_request_id -> run_id correlation map, and pumps mapped events
// onto the Events() channel. run_id never flows through the core.AgentSession
// interface, so all of that tracking is internal (implementation plan §5).
type session struct {
	opts         options
	client       *rpcClient
	ccSessionKey string
	resumeID     string // route session_id to resume (StartSession arg)

	events chan core.Event
	alive  atomic.Bool

	mu          sync.Mutex
	sessionID   string            // current route session_id
	activeRunID string            // run_id of the in-flight turn, "" if none
	permRunIDs  map[string]string // permission_request_id -> run_id
	seenEvents  map[string]bool   // event_id dedup set

	closeOnce sync.Once
}

// newSession builds a session. resumeSessionID is the route session_id the
// engine asks to resume (may be empty); ccSessionKey is the local IM/platform
// session key captured from CC_SESSION_KEY.
func newSession(opts options, client *rpcClient, resumeSessionID, ccSessionKey string) *session {
	return &session{
		opts:         opts,
		client:       client,
		ccSessionKey: ccSessionKey,
		resumeID:     resumeSessionID,
		events:       make(chan core.Event, 64),
		permRunIDs:   map[string]string{},
		seenEvents:   map[string]bool{},
	}
}

// start issues session.start and, on success, begins pumping events.
func (s *session) start(ctx context.Context) error {
	params := sessionStartParams{
		RequestID:          newRequestID(),
		CCConnectSessionID: s.ccSessionKey,
		Project:            s.opts.project,
		Workspace:          s.opts.workspace,
		DefaultAgent:       s.opts.defaultAgent,
	}
	// resume_session_id is sent only when resume is enabled and the engine
	// handed us a prior route session_id (see implementation plan §10).
	if s.opts.resume && s.resumeID != "" {
		params.ResumeSessionID = s.resumeID
	}

	var res sessionStartResult
	if err := s.client.call(ctx, methodSessionStart, params, &res); err != nil {
		return fmt.Errorf("agentroute: session.start: %w", err)
	}

	s.mu.Lock()
	s.sessionID = res.SessionID
	if s.sessionID == "" {
		s.sessionID = s.resumeID
	}
	s.mu.Unlock()

	s.alive.Store(true)
	go s.pump()
	return nil
}

// Send delivers one user turn. It returns once the turn is accepted, not once
// it completes; output streams asynchronously on Events(). Send requires a
// live connection and never re-dials — reconnect goes through the engine's
// dead-session path (implementation plan §10).
func (s *session) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.alive.Load() {
		return errors.New("agentroute: session is not connected")
	}

	// Generate the run_id before sending so the active run is known
	// deterministically before the session.send response arrives.
	runID := newRunID()
	s.mu.Lock()
	s.activeRunID = runID
	sessionID := s.sessionID
	s.mu.Unlock()

	imgs := attachmentRefsFromImages(images)
	if imgs == nil {
		imgs = []attachmentRef{}
	}
	fls := attachmentRefsFromFiles(files)
	if fls == nil {
		fls = []attachmentRef{}
	}

	params := sessionSendParams{
		RequestID: newRequestID(),
		SessionID: sessionID,
		RunID:     runID,
		Prompt:    prompt,
		Images:    imgs,
		Files:     fls,
	}
	var res sessionSendResult
	if err := s.client.call(context.Background(), methodSessionSend, params, &res); err != nil {
		return fmt.Errorf("agentroute: session.send: %w", err)
	}
	if res.RunID != "" && res.RunID != runID {
		return fmt.Errorf("agentroute: session.send echoed run_id %q, expected %q", res.RunID, runID)
	}
	// accepted=false means the turn was refused and no events will stream for
	// it; fail the call so the engine does not wait for output that never comes.
	if !res.Accepted {
		return fmt.Errorf("agentroute: session.send for run %q was not accepted by agent-route (status %q)", runID, res.Status)
	}
	return nil
}

// RespondPermission answers a pending permission request. The engine passes
// only the permission_request_id; the run_id is recovered from the
// correlation map populated when the permission_request event arrived.
func (s *session) RespondPermission(requestID string, result core.PermissionResult) error {
	s.mu.Lock()
	runID, ok := s.permRunIDs[requestID]
	sessionID := s.sessionID
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("agentroute: no run_id for permission request %q (expired or unknown)", requestID)
	}

	behavior, updated, message := mapPermissionResult(result)
	params := permissionRespondParams{
		RequestID:           newRequestID(),
		SessionID:           sessionID,
		RunID:               runID,
		PermissionRequestID: requestID,
		Behavior:            behavior,
		UpdatedInput:        updated,
		Message:             message,
	}
	var res permissionRespondResult
	if err := s.client.call(context.Background(), methodPermissionRespond, params, &res); err != nil {
		return fmt.Errorf("agentroute: permission.respond: %w", err)
	}
	if !res.Accepted {
		return fmt.Errorf("agentroute: permission.respond for %q was not accepted by agent-route", requestID)
	}
	// Drop the correlation only after the server confirms the answer, so a
	// failed or rejected attempt leaves the run_id mapping intact for a retry.
	s.mu.Lock()
	delete(s.permRunIDs, requestID)
	s.mu.Unlock()
	return nil
}

// Events returns the event channel, kept open across turns.
func (s *session) Events() <-chan core.Event { return s.events }

// CurrentSessionID returns the route session_id, persisted by the engine as
// the agent session ID.
func (s *session) CurrentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Alive reports whether the connection is still usable. It flips to false the
// moment the connection is lost or Close ran, so the engine recycles the
// state and reconnects via StartSession. It also consults the rpc client
// directly so a failed write is reflected immediately, without waiting for the
// pump goroutine to observe the disconnect.
func (s *session) Alive() bool {
	if !s.alive.Load() {
		return false
	}
	select {
	case <-s.client.Done():
		return false
	default:
		return true
	}
}

// Close cancels the active run (when known), closes the route session, and
// tears down the websocket. It is safe to call more than once.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)

		s.mu.Lock()
		runID := s.activeRunID
		sessionID := s.sessionID
		s.activeRunID = ""
		s.mu.Unlock()

		// Best-effort cancel of an in-flight run, bounded by closeRPCTimeout
		// (not the configurable request timeout) so a slow or unresponsive
		// agent-route cannot stall shutdown.
		if runID != "" {
			var res sessionCancelResult
			_ = s.client.callWithTimeout(context.Background(), closeRPCTimeout, methodSessionCancel, sessionCancelParams{
				RequestID: newRequestID(),
				SessionID: sessionID,
				RunID:     runID,
				Reason:    "user_cancelled",
			}, &res)
		}

		var closeRes sessionCloseResult
		_ = s.client.callWithTimeout(context.Background(), closeRPCTimeout, methodSessionClose, sessionCloseParams{
			RequestID: newRequestID(),
			SessionID: sessionID,
			Reason:    "client_session_closed",
		}, &closeRes)

		// Always tear the websocket down, even if the RPCs above timed out.
		_ = s.client.Close()
	})
	return nil
}

// pump is the single goroutine that consumes server notifications and turns
// them into core.Event values. It also owns disconnect detection.
func (s *session) pump() {
	events := s.client.events
	for {
		select {
		case note := <-events:
			s.handleNotification(note)
		case <-s.client.Done():
			// Drain anything already buffered, then signal the disconnect.
			for {
				select {
				case note := <-events:
					s.handleNotification(note)
				default:
					s.handleDisconnect()
					return
				}
			}
		}
	}
}

// handleNotification deduplicates by event_id, maps the event, maintains the
// run_id correlation state, and forwards the result.
func (s *session) handleNotification(note sessionEventNotification) {
	if note.EventID != "" {
		s.mu.Lock()
		dup := s.seenEvents[note.EventID]
		if !dup {
			s.seenEvents[note.EventID] = true
		}
		s.mu.Unlock()
		if dup {
			return
		}
	}

	var pe protocolEvent
	if err := json.Unmarshal(note.Event, &pe); err != nil {
		slog.Warn("agentroute: discarding malformed session.event", "error", err)
		return
	}

	ev, emit, terminal := mapEvent(pe)

	// Correlate a permission request to the run it belongs to BEFORE the
	// event is emitted, so a fast RespondPermission cannot race the map.
	if pe.Type == "permission_request" && pe.PermissionRequestID != "" {
		s.mu.Lock()
		s.permRunIDs[pe.PermissionRequestID] = note.RunID
		s.mu.Unlock()
	}

	// A terminal event (result / cancelled / terminal error) clears the
	// active run. A non-terminal error must NOT reach here as terminal.
	if terminal {
		s.clearActiveRun(note.RunID)
	}

	if !emit {
		return
	}
	if ev.Type == core.EventResult && ev.SessionID == "" {
		ev.SessionID = s.CurrentSessionID()
	}
	s.emit(ev)
}

// clearActiveRun clears the active run_id when the terminal event belongs to
// it. A terminal event for some other (stale) run must not clobber a newer
// active run.
func (s *session) clearActiveRun(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID == "" || s.activeRunID == runID {
		s.activeRunID = ""
	}
}

// handleDisconnect runs once when the connection is lost. For an unexpected
// loss it emits a single EventError so the in-flight turn ends cleanly; a
// graceful Close emits nothing.
func (s *session) handleDisconnect() {
	if !s.alive.Swap(false) {
		return // Close already ran, or disconnect already handled
	}
	reason := s.client.closeReason()
	if errors.Is(reason, errClientClosed) {
		return
	}
	s.emit(core.Event{
		Type:  core.EventError,
		Error: fmt.Errorf("agentroute: connection lost: %w", reason),
	})
}

// emit forwards one event, blocking on the buffered channel but bailing out
// if the connection is already gone.
func (s *session) emit(ev core.Event) {
	select {
	case s.events <- ev:
	case <-s.client.Done():
		// Connection gone: still try a non-blocking send so a terminal
		// event is not lost while the buffer has room.
		select {
		case s.events <- ev:
		default:
		}
	}
}
