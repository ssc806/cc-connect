package youzone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// tokenSource records where the cached token came from, for log fields only.
type tokenSource string

const (
	tokenSourceStatic tokenSource = "static"
	tokenSourceHelper tokenSource = "helper"
	tokenSourceChrome tokenSource = "chrome"
)

// refreshReason names why a refresh ran, for log fields only.
type refreshReason string

const (
	refreshReasonEmpty      refreshReason = "empty"       // no cached token yet
	refreshReasonExpiring   refreshReason = "expiring"    // cached token entered the refresh window
	refreshReasonAuthFailed refreshReason = "auth_failed" // server rejected the current token
)

// refreshFailureCooldown throttles proactive (non-forced) refreshes after a
// helper failure, so a broken helper is not re-spawned on every outbound
// request. Forced (auth-failure) refreshes ignore this and always try.
const refreshFailureCooldown = 30 * time.Second

// helperStderrCap bounds how much helper stderr is folded into an error
// message. The full stderr is never logged.
const helperStderrCap = 256

// helperRunner executes the access-token helper. It is a tokenManager field so
// tests can substitute a fake instead of spawning real processes.
type helperRunner func(ctx context.Context, argv []string, timeout time.Duration) (stdout, stderr []byte, err error)

// tokenSourceRunner executes a built-in access-token source.
type tokenSourceRunner func(ctx context.Context, source string) (helperOutput, error)

// tokenManager owns the YouZone yht_access_token: it serves the cached token,
// refreshes it through either an external helper or a built-in token source
// when configured, and keeps the previous token around purely so it can still
// be redacted out of logs after a refresh.
//
// Concurrency: mu is held for the entire refresh, including the helper exec.
// That guarantees at most one helper process at a time; concurrent callers
// block on mu and, once it is released, re-check the cache — so a burst of
// requests collapses into a single helper invocation.
type tokenManager struct {
	helper            []string // argv; empty => static-only, no refresh
	accessTokenSource string
	helperTimeout     time.Duration
	ttl               time.Duration // expiry fallback when the helper returns none
	refreshBefore     time.Duration // proactive-refresh lead time before expiry

	runHelper helperRunner      // overridable in tests
	runSource tokenSourceRunner // overridable in tests
	now       func() time.Time  // overridable in tests

	mu             sync.Mutex
	token          string
	prevToken      string
	expiresAt      time.Time
	source         tokenSource
	lastRefreshErr error
	lastRefreshAt  time.Time
}

// newTokenManager builds a tokenManager from config. A static access_token (if
// any) seeds the cache so the first request can go out before the helper has
// ever run; it is aged from process start by ttl so it still enters the
// refresh window when a helper is configured.
func newTokenManager(cfg config) *tokenManager {
	tm := &tokenManager{
		helper:            cfg.accessTokenHelper,
		accessTokenSource: cfg.accessTokenSource,
		helperTimeout:     cfg.accessTokenHelperTimeout,
		ttl:               cfg.accessTokenTTL,
		refreshBefore:     cfg.accessTokenRefreshBefore,
		runHelper:         runHelperProcess,
		runSource:         runBuiltInTokenSource,
		now:               time.Now,
	}
	if cfg.accessToken != "" {
		tm.token = cfg.accessToken
		tm.source = tokenSourceStatic
		tm.expiresAt = tm.now().Add(tm.ttl)
	}
	return tm
}

// Token returns the access token for the next request.
//
// force=false (normal request): returns the cached token unless it is empty or
// has entered the refresh window, in which case the helper runs.
//
// force=true (after an auth failure): always attempts a helper refresh and
// never falls back to the rejected token — the server already deemed it
// invalid, so reusing it would just fetch the CAS login page again.
func (tm *tokenManager) Token(ctx context.Context, force bool) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	now := tm.now()

	if !force {
		if tm.token != "" && now.Before(tm.refreshDeadlineLocked()) {
			return tm.token, nil
		}
		if !tm.canRefreshLocked() {
			// Static-only manager: a single token, never refreshed — identical
			// to cc-connect's pre-helper behavior.
			if tm.token != "" {
				return tm.token, nil
			}
			return "", fmt.Errorf("youzone: no access token configured")
		}
		// Throttle: skip the helper shortly after it failed, reusing the cached
		// token while it is still usable rather than re-spawning on every
		// message.
		if tm.lastRefreshErr != nil && now.Sub(tm.lastRefreshAt) < refreshFailureCooldown {
			if tm.token != "" && now.Before(tm.expiresAt) {
				return tm.token, nil
			}
			return "", fmt.Errorf("youzone: access token refresh failed: %w", tm.lastRefreshErr)
		}
	}

	if !tm.canRefreshLocked() {
		// force=true with nothing to refresh with. Reporting the failure (rather
		// than returning the rejected token) lets the caller stop retrying.
		return "", fmt.Errorf("youzone: access token rejected and no access_token_helper or access_token_source is configured to refresh it")
	}

	return tm.refreshLocked(ctx, tm.classifyReasonLocked(force))
}

// refreshDeadlineLocked is the instant at which a cached token is considered
// "expiring" and a proactive refresh should run.
func (tm *tokenManager) refreshDeadlineLocked() time.Time {
	return tm.expiresAt.Add(-tm.refreshBefore)
}

func (tm *tokenManager) canRefreshLocked() bool {
	return len(tm.helper) > 0 || tm.accessTokenSource != ""
}

func (tm *tokenManager) classifyReasonLocked(force bool) refreshReason {
	switch {
	case force:
		return refreshReasonAuthFailed
	case tm.token == "":
		return refreshReasonEmpty
	default:
		return refreshReasonExpiring
	}
}

// refreshLocked runs the helper once and updates the cache. mu must be held.
func (tm *tokenManager) refreshLocked(ctx context.Context, reason refreshReason) (string, error) {
	start := tm.now()
	slog.Info("youzone: access token refresh started", "reason", string(reason))

	out, err := tm.runRefreshProviderLocked(ctx)
	elapsed := tm.now().Sub(start)
	if err != nil {
		return tm.refreshFailedLocked(reason, elapsed, err)
	}

	tm.prevToken = tm.token
	tm.token = out.token
	tm.expiresAt = out.expiresAt
	tm.source = tm.refreshSourceLocked()
	tm.lastRefreshErr = nil
	tm.lastRefreshAt = tm.now()

	slog.Info("youzone: access token refresh succeeded",
		"reason", string(reason),
		"source", string(tm.source),
		"elapsed", elapsed,
		"expires_in", out.expiresAt.Sub(tm.now()).Round(time.Second),
	)
	return tm.token, nil
}

func (tm *tokenManager) refreshSourceLocked() tokenSource {
	if len(tm.helper) > 0 {
		return tokenSourceHelper
	}
	if tm.accessTokenSource == accessTokenSourceChrome {
		return tokenSourceChrome
	}
	return tokenSource(tm.accessTokenSource)
}

func (tm *tokenManager) runRefreshProviderLocked(ctx context.Context) (helperOutput, error) {
	if len(tm.helper) > 0 {
		stdout, stderr, err := tm.runHelper(ctx, tm.helper, tm.helperTimeout)
		if err != nil {
			return helperOutput{}, helperError(err, stderr)
		}
		out, err := parseHelperOutput(stdout, tm.now(), tm.ttl)
		if err != nil {
			return helperOutput{}, err
		}
		return out, nil
	}
	out, err := tm.runSource(ctx, tm.accessTokenSource)
	if err != nil {
		return helperOutput{}, fmt.Errorf("%s token source: %w", tm.accessTokenSource, err)
	}
	if out.token == "" {
		return helperOutput{}, fmt.Errorf("%s token source produced empty token", tm.accessTokenSource)
	}
	if out.expiresAt.IsZero() {
		out.expiresAt = tm.now().Add(tm.ttl)
	}
	return out, nil
}

// refreshFailedLocked records a helper failure and decides whether the caller
// can keep using the cached token. mu must be held. The cause is redacted
// once, here, so neither the stored lastRefreshErr nor the returned error can
// leak a token from helper stderr into a caller's logs.
func (tm *tokenManager) refreshFailedLocked(reason refreshReason, elapsed time.Duration, cause error) (string, error) {
	safe := fmt.Errorf("%s", tm.redactLocked(cause.Error()))
	tm.lastRefreshErr = safe
	tm.lastRefreshAt = tm.now()
	slog.Warn("youzone: access token refresh failed",
		"reason", string(reason),
		"elapsed", elapsed,
		"err", safe.Error(),
	)
	// auth_failed: the server already rejected the current token; handing it
	// back would just fetch the same login page. Surface the error instead.
	if reason == refreshReasonAuthFailed {
		return "", fmt.Errorf("youzone: access token refresh failed: %w", safe)
	}
	// Proactive refresh (empty/expiring): if the cached token has not yet hard
	// expired, keep using it so a transient helper outage does not take the
	// platform down.
	if tm.token != "" && tm.now().Before(tm.expiresAt) {
		return tm.token, nil
	}
	return "", fmt.Errorf("youzone: access token refresh failed: %w", safe)
}

// Redact removes the current and previous access tokens from text. The
// previous token is scrubbed too so a stale token captured in an in-flight
// response body is still hidden after a refresh has rotated it out.
func (tm *tokenManager) Redact(text string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.redactLocked(text)
}

func (tm *tokenManager) redactLocked(text string) string {
	text = core.RedactToken(text, tm.token)
	text = core.RedactToken(text, tm.prevToken)
	return text
}

// helperOutput is the parsed result of a successful helper run.
type helperOutput struct {
	token     string
	expiresAt time.Time
}

// parseHelperOutput interprets the helper's stdout. Trimmed output starting
// with '{' is parsed as JSON; anything else is the bare token. In JSON,
// expires_at (RFC3339) wins over expires_in (whole seconds); when the helper
// supplies neither, the configured ttl is used.
func parseHelperOutput(stdout []byte, now time.Time, ttl time.Duration) (helperOutput, error) {
	s := strings.TrimSpace(string(stdout))
	if s == "" {
		return helperOutput{}, fmt.Errorf("helper produced empty output")
	}
	if !strings.HasPrefix(s, "{") {
		return helperOutput{token: s, expiresAt: now.Add(ttl)}, nil
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   *int64 `json:"expires_in"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return helperOutput{}, fmt.Errorf("helper output is not valid JSON: %w", err)
	}
	token := strings.TrimSpace(parsed.AccessToken)
	if token == "" {
		return helperOutput{}, fmt.Errorf("helper JSON is missing a non-empty access_token")
	}

	out := helperOutput{token: token}
	switch {
	case strings.TrimSpace(parsed.ExpiresAt) != "":
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(parsed.ExpiresAt))
		if err != nil {
			return helperOutput{}, fmt.Errorf("helper expires_at is not RFC3339: %w", err)
		}
		out.expiresAt = t
	case parsed.ExpiresIn != nil:
		out.expiresAt = now.Add(time.Duration(*parsed.ExpiresIn) * time.Second)
	default:
		out.expiresAt = now.Add(ttl)
	}
	return out, nil
}

// helperError folds a helper exec error and its (length-capped) stderr into a
// single error. Token redaction is applied by the caller, which knows the
// current/previous tokens.
func helperError(err error, stderr []byte) error {
	msg := err.Error()
	if s := strings.TrimSpace(string(stderr)); s != "" {
		if len(s) > helperStderrCap {
			s = s[:helperStderrCap] + "..."
		}
		msg = msg + ": " + s
	}
	return fmt.Errorf("helper: %s", msg)
}

// runHelperProcess executes the helper through exec.CommandContext — never a
// shell. argv[0] is the executable; argv[1:] are literal arguments. A run that
// outlives timeout is killed and reported as a timeout.
func runHelperProcess(ctx context.Context, argv []string, timeout time.Duration) (stdout, stderr []byte, err error) {
	if len(argv) == 0 {
		return nil, nil, fmt.Errorf("helper command is empty")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("helper timed out after %s", timeout)
	}
	return outBuf.Bytes(), errBuf.Bytes(), runErr
}
