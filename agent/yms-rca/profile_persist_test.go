package ymsagent

import (
	"path/filepath"
	"testing"
)

// newTestSessionWithStore wires a fresh per-test profileStore + project +
// sessionKey onto a non-subprocess session.
func newTestSessionWithStore(t *testing.T, project, sessionKey string) (*session, *profileStore) {
	t.Helper()
	s, _ := newTestSession(t, "default")
	store := newProfileStore(filepath.Join(t.TempDir(), "store.json"))
	s.profileStore = store
	s.project = project
	s.sessionKey = sessionKey
	return s, store
}

func TestUpdateCurrentProfilePersistsNonLocalProfile(t *testing.T) {
	s, store := newTestSessionWithStore(t, "yms-rca-youzone", "youzone:conv:user")

	s.updateCurrentProfile("pre")

	if got := store.Get("yms-rca-youzone", "youzone:conv:user"); got != "pre" {
		t.Errorf("store.Get = %q, want pre", got)
	}
	if got := s.currentProfileName(); got != "pre" {
		t.Errorf("currentProfileName = %q, want pre", got)
	}
}

func TestUpdateCurrentProfileClearsLocalProfile(t *testing.T) {
	s, store := newTestSessionWithStore(t, "yms-rca-youzone", "youzone:conv:user")
	store.Set("yms-rca-youzone", "youzone:conv:user", "pre")

	s.updateCurrentProfile("local")

	if got := store.Get("yms-rca-youzone", "youzone:conv:user"); got != "" {
		t.Errorf("after switch to local, store should be cleared, got %q", got)
	}
	if got := s.currentProfileName(); got != "local" {
		t.Errorf("currentProfileName = %q, want local", got)
	}
}

func TestUpdateCurrentProfileNoStoreIsNoOp(t *testing.T) {
	// Without a store wired (e.g. older code path / unit-test session), the
	// existing in-memory profile update path must still work.
	s, _ := newTestSession(t, "default")
	s.updateCurrentProfile("pre")
	if got := s.currentProfileName(); got != "pre" {
		t.Errorf("currentProfileName = %q, want pre", got)
	}
}

func TestUpdateCurrentProfileNoProjectOrSessionKeyIsNoOp(t *testing.T) {
	// If extraEnv didn't carry CC_PROJECT/CC_SESSION_KEY (programmatic test,
	// CLI relay not attached), store stays untouched but in-memory profile
	// still updates.
	s, store := newTestSessionWithStore(t, "", "")
	s.updateCurrentProfile("pre")
	if got := s.currentProfileName(); got != "pre" {
		t.Errorf("currentProfileName = %q, want pre", got)
	}
	// store has nothing under "" keys (Set rejects empty)
	if got := store.Get("", ""); got != "" {
		t.Errorf("store should reject empty keys, got %q", got)
	}
}

func TestNewSessionExtractsProjectAndSessionKeyFromEnv(t *testing.T) {
	// Using the buildSessionEnv pure helper isn't enough — we need the
	// session struct's project/sessionKey populated from extraEnv. Verify
	// via the parser used by newSession.
	project, sessionKey := parseProjectAndSessionKey([]string{
		"PATH=/usr/bin",
		"CC_PROJECT=yms-rca-youzone",
		"CC_SESSION_KEY=youzone:claw_123:5837619",
		"OTHER=value",
	})
	if project != "yms-rca-youzone" {
		t.Errorf("project = %q, want yms-rca-youzone", project)
	}
	if sessionKey != "youzone:claw_123:5837619" {
		t.Errorf("sessionKey = %q, want youzone:claw_123:5837619", sessionKey)
	}
}

// TestObserveStatusProfileDoesNotClearStore verifies the PR #10 review-
// round-2 fix for Finding 2b: a setStatus echo of "env: local" from
// /status (or any informational subprocess status update) must NOT
// erase the persisted non-local profile. Only the authoritative env-
// switch path (yms-rca.env-switch message_end) — i.e. the user really
// ran /disconnect — may clear the store.
func TestObserveStatusProfileDoesNotClearStore(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")
	store.Set("p", "k", "pre")

	// Simulate /status echoing back "env: local" because the subprocess
	// is freshly spawned and the auto-restore hasn't run yet.
	s.observeStatusProfile("local")

	if got := store.Get("p", "k"); got != "pre" {
		t.Errorf("setStatus echo must not clear store; want pre, got %q", got)
	}
	if got := s.currentProfileName(); got != "local" {
		t.Errorf("in-memory profile should reflect echo, got %q", got)
	}
}

// TestObserveStatusProfileUpdatesInMemoryButDoesNotPersist verifies the
// inverse — observing a non-local status echo updates the in-memory
// snapshot but still does not touch the store (the env-switch path is
// authoritative for persistence).
func TestObserveStatusProfileUpdatesInMemoryButDoesNotPersist(t *testing.T) {
	s, store := newTestSessionWithStore(t, "p", "k")

	s.observeStatusProfile("yms-dev")

	if got := s.currentProfileName(); got != "yms-dev" {
		t.Errorf("in-memory profile = %q, want yms-dev", got)
	}
	if got := store.Get("p", "k"); got != "" {
		t.Errorf("setStatus must not persist; store = %q, want empty", got)
	}
}

// TestProfileStorePersistsOnlyAuthoritativeEnvSwitch is the cross-confirming
// regression for Phase 2: setStatus and env-switch both flip profileObserved
// (so the footer can render the current subprocess's truth), but only
// env-switch is allowed to persist the (project, session_key) → profile entry.
func TestProfileStorePersistsOnlyAuthoritativeEnvSwitch(t *testing.T) {
	t.Run("setStatus updates footer state but does not persist", func(t *testing.T) {
		s, store := newTestSessionWithStore(t, "p", "k")
		s.observeStatusProfile("yms-dev")
		if !s.profileObserved.Load() {
			t.Error("setStatus must mark profile observed (footer-gate-on)")
		}
		if got := store.Get("p", "k"); got != "" {
			t.Errorf("setStatus must not persist; store = %q", got)
		}
	})

	t.Run("env-switch updates footer state AND persists", func(t *testing.T) {
		s, store := newTestSessionWithStore(t, "p", "k")
		s.updateCurrentProfile("yms-dev")
		if !s.profileObserved.Load() {
			t.Error("env-switch must mark profile observed (footer-gate-on)")
		}
		if got := store.Get("p", "k"); got != "yms-dev" {
			t.Errorf("env-switch must persist; store = %q, want yms-dev", got)
		}
	})

	t.Run("env-switch to local clears persisted entry", func(t *testing.T) {
		s, store := newTestSessionWithStore(t, "p", "k")
		store.Set("p", "k", "yms-dev")
		s.updateCurrentProfile("local")
		if got := store.Get("p", "k"); got != "" {
			t.Errorf("env-switch local must clear store; got %q", got)
		}
	})
}

func TestParseProjectAndSessionKeyMissingFields(t *testing.T) {
	project, sessionKey := parseProjectAndSessionKey([]string{"PATH=/usr/bin"})
	if project != "" || sessionKey != "" {
		t.Errorf("missing env should yield empty, got project=%q key=%q", project, sessionKey)
	}
}
