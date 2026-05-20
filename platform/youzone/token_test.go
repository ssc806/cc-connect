package youzone

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic time source so token-expiry logic can be
// exercised without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)}
}

// newTestTokenManager builds a tokenManager wired to a fake clock. ttl is 1h
// and refreshBefore is 10m so the proactive-refresh window is easy to reason
// about in tests. Callers set .runHelper before exercising refreshes.
func newTestTokenManager(clk *fakeClock, helper []string, static string) *tokenManager {
	tm := &tokenManager{
		helper:        helper,
		helperTimeout: 10 * time.Second,
		ttl:           time.Hour,
		refreshBefore: 10 * time.Minute,
		now:           clk.now,
	}
	if static != "" {
		tm.token = static
		tm.source = tokenSourceStatic
		tm.expiresAt = clk.now().Add(tm.ttl)
	}
	return tm
}

// stubHelper returns a runHelper that always yields the given stdout and counts
// invocations.
func stubHelper(stdout string, calls *int32) helperRunner {
	return func(_ context.Context, _ []string, _ time.Duration) ([]byte, []byte, error) {
		atomic.AddInt32(calls, 1)
		return []byte(stdout), nil, nil
	}
}

func TestParseHelperOutputPlainText(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	out, err := parseHelperOutput([]byte("  raw-token-value\n"), now, 2*time.Hour)
	if err != nil {
		t.Fatalf("parseHelperOutput() error = %v", err)
	}
	if out.token != "raw-token-value" {
		t.Errorf("token = %q, want raw-token-value", out.token)
	}
	if !out.expiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("expiresAt = %v, want now+ttl", out.expiresAt)
	}
}

func TestParseHelperOutputJSONWithoutExpiry(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	out, err := parseHelperOutput([]byte(`{"access_token":"json-token"}`), now, 3*time.Hour)
	if err != nil {
		t.Fatalf("parseHelperOutput() error = %v", err)
	}
	if out.token != "json-token" {
		t.Errorf("token = %q", out.token)
	}
	if !out.expiresAt.Equal(now.Add(3 * time.Hour)) {
		t.Errorf("expiresAt = %v, want now+ttl fallback", out.expiresAt)
	}
}

func TestParseHelperOutputExpiresIn(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	out, err := parseHelperOutput([]byte(`{"access_token":"t","expires_in":54000}`), now, time.Hour)
	if err != nil {
		t.Fatalf("parseHelperOutput() error = %v", err)
	}
	if !out.expiresAt.Equal(now.Add(54000 * time.Second)) {
		t.Errorf("expiresAt = %v, want now+54000s", out.expiresAt)
	}
}

func TestParseHelperOutputExpiresAtWinsOverExpiresIn(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	out, err := parseHelperOutput(
		[]byte(`{"access_token":"t","expires_in":60,"expires_at":"2026-05-20T23:10:00Z"}`),
		now, time.Hour)
	if err != nil {
		t.Fatalf("parseHelperOutput() error = %v", err)
	}
	want := time.Date(2026, 5, 20, 23, 10, 0, 0, time.UTC)
	if !out.expiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (expires_at must win)", out.expiresAt, want)
	}
}

func TestParseHelperOutputRejectsBadInput(t *testing.T) {
	now := time.Now()
	cases := map[string]string{
		"empty":              "   \n ",
		"json missing token": `{"expires_in":60}`,
		"json empty token":   `{"access_token":"  "}`,
		"malformed json":     `{not json`,
		"bad expires_at":     `{"access_token":"t","expires_at":"not-a-time"}`,
	}
	for name, stdout := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseHelperOutput([]byte(stdout), now, time.Hour); err == nil {
				t.Fatalf("parseHelperOutput(%q) error = nil, want failure", stdout)
			}
		})
	}
}

func TestNewTokenManagerSeedsStaticToken(t *testing.T) {
	cfg := config{
		accessToken:              "static-token",
		accessTokenHelperTimeout: 10 * time.Second,
		accessTokenTTL:           time.Hour,
		accessTokenRefreshBefore: 10 * time.Minute,
	}
	tm := newTokenManager(cfg)
	tok, err := tm.Token(context.Background(), false)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if tok != "static-token" {
		t.Errorf("Token() = %q, want static-token", tok)
	}
	if tm.source != tokenSourceStatic {
		t.Errorf("source = %q, want static", tm.source)
	}
}

func TestTokenManagerStaticOnlyNeverRefreshes(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, nil, "static-token")

	// Even far past the nominal expiry, a helper-less manager keeps the token.
	clk.advance(48 * time.Hour)
	tok, err := tm.Token(context.Background(), false)
	if err != nil || tok != "static-token" {
		t.Fatalf("Token() = %q, %v; want static-token, nil", tok, err)
	}

	// A forced refresh has nothing to refresh with and must report that rather
	// than silently return the server-rejected token.
	if _, err := tm.Token(context.Background(), true); err == nil {
		t.Fatal("forced Token() error = nil, want failure (no helper configured)")
	}
}

func TestTokenManagerLazyLoadsViaHelper(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")
	var calls int32
	tm.runHelper = stubHelper("fresh-token", &calls)

	tok, err := tm.Token(context.Background(), false)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if tok != "fresh-token" {
		t.Errorf("Token() = %q, want fresh-token", tok)
	}
	if calls != 1 {
		t.Errorf("helper invoked %d times, want 1", calls)
	}
}

func TestTokenManagerCachesUntilRefreshWindow(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")
	var calls int32
	tm.runHelper = stubHelper("fresh-token", &calls)

	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	// Still well outside the refresh window (ttl 1h, refreshBefore 10m).
	clk.advance(30 * time.Minute)
	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if calls != 1 {
		t.Errorf("helper invoked %d times, want 1 (cached token reused)", calls)
	}
}

func TestTokenManagerProactiveRefreshInsideWindow(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")
	var calls int32
	tm.runHelper = stubHelper("fresh-token", &calls)

	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	// Cross into the 10m refresh-before window.
	clk.advance(55 * time.Minute)
	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("helper invoked %d times, want 2 (proactive refresh)", calls)
	}
}

func TestTokenManagerForcedRefreshAlwaysRunsHelper(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")
	var calls int32
	tm.runHelper = stubHelper("fresh-token", &calls)

	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	// Token is fresh, but a forced (auth-failure) refresh must still run.
	if _, err := tm.Token(context.Background(), true); err != nil {
		t.Fatalf("forced Token() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("helper invoked %d times, want 2 (forced refresh ignores cache)", calls)
	}
}

func TestTokenManagerForcedRefreshFailureDoesNotFallBackToStatic(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "static-token")
	tm.runHelper = func(_ context.Context, _ []string, _ time.Duration) ([]byte, []byte, error) {
		return nil, []byte("chrome not logged in"), errors.New("exit status 2")
	}

	tok, err := tm.Token(context.Background(), true)
	if err == nil {
		t.Fatal("forced Token() error = nil, want failure")
	}
	if tok == "static-token" {
		t.Fatal("forced refresh fell back to the server-rejected static token")
	}
}

func TestTokenManagerProactiveRefreshFailureKeepsUsableToken(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "static-token")
	tm.runHelper = func(_ context.Context, _ []string, _ time.Duration) ([]byte, []byte, error) {
		return nil, nil, errors.New("helper unavailable")
	}

	// Inside the refresh window but not yet hard-expired: the proactive refresh
	// fails, yet the still-valid cached token should keep the platform alive.
	clk.advance(55 * time.Minute)
	tok, err := tm.Token(context.Background(), false)
	if err != nil {
		t.Fatalf("Token() error = %v, want cached token on soft failure", err)
	}
	if tok != "static-token" {
		t.Errorf("Token() = %q, want cached static-token", tok)
	}
	if tm.lastRefreshErr == nil {
		t.Error("lastRefreshErr not recorded after helper failure")
	}
}

func TestTokenManagerThrottlesHelperAfterFailure(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "static-token")
	var calls int32
	tm.runHelper = func(_ context.Context, _ []string, _ time.Duration) ([]byte, []byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil, errors.New("helper unavailable")
	}

	clk.advance(55 * time.Minute) // into refresh window
	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	// A second proactive call moments later must not re-spawn the helper.
	clk.advance(2 * time.Second)
	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if calls != 1 {
		t.Errorf("helper invoked %d times, want 1 (failure throttled)", calls)
	}
}

func TestTokenManagerEmptyHelperOutputIsFailure(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")
	tm.runHelper = func(_ context.Context, _ []string, _ time.Duration) ([]byte, []byte, error) {
		return []byte("   "), nil, nil
	}
	if _, err := tm.Token(context.Background(), false); err == nil {
		t.Fatal("Token() error = nil, want failure on empty helper output")
	}
}

func TestTokenManagerConcurrentRefreshMergesToOneHelperCall(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")
	var calls int32
	tm.runHelper = func(_ context.Context, _ []string, _ time.Duration) ([]byte, []byte, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond) // widen the window for goroutines to pile up
		return []byte("fresh-token"), nil, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tm.Token(context.Background(), false); err != nil {
				t.Errorf("Token() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Errorf("helper invoked %d times, want 1 (concurrent refreshes merged)", calls)
	}
}

func TestTokenManagerRedactsCurrentAndPreviousToken(t *testing.T) {
	clk := newFakeClock()
	tm := newTestTokenManager(clk, []string{"/helper"}, "")

	var calls int32
	tm.runHelper = stubHelper("token-AAA", &calls)
	if _, err := tm.Token(context.Background(), false); err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	tm.runHelper = stubHelper("token-BBB", &calls)
	if _, err := tm.Token(context.Background(), true); err != nil {
		t.Fatalf("forced Token() error = %v", err)
	}

	got := tm.Redact("body has token-AAA and token-BBB inside")
	if strings.Contains(got, "token-AAA") {
		t.Errorf("previous token leaked: %q", got)
	}
	if strings.Contains(got, "token-BBB") {
		t.Errorf("current token leaked: %q", got)
	}
}

func TestRunHelperProcessCapturesStdout(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not available")
	}
	stdout, _, err := runHelperProcess(context.Background(), []string{echo, "hello-token"}, 5*time.Second)
	if err != nil {
		t.Fatalf("runHelperProcess() error = %v", err)
	}
	if strings.TrimSpace(string(stdout)) != "hello-token" {
		t.Errorf("stdout = %q, want hello-token", stdout)
	}
}

func TestRunHelperProcessTimesOut(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	start := time.Now()
	_, _, err = runHelperProcess(context.Background(), []string{sleep, "10"}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("runHelperProcess() error = nil, want timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("runHelperProcess did not honour the timeout: took %s", time.Since(start))
	}
}

func TestRunHelperProcessNonZeroExitCarriesStderr(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	// `sleep` with a non-numeric argument exits non-zero and writes to stderr.
	stdout, stderr, err := runHelperProcess(context.Background(), []string{sleep, "not-a-number"}, 5*time.Second)
	if err == nil {
		t.Fatal("runHelperProcess() error = nil, want non-zero exit")
	}
	if len(stdout) != 0 {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if len(stderr) == 0 {
		t.Error("stderr not captured for a failing helper")
	}
}
