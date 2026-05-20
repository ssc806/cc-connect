package agentroute

import (
	"strings"
	"testing"
	"time"
)

func TestParseOptions_RequiresURL(t *testing.T) {
	_, err := parseOptions(map[string]any{"token": "secret"})
	if err == nil {
		t.Fatal("expected error when url is missing, got nil")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Fatalf("error should mention url, got %q", err.Error())
	}
}

func TestParseOptions_RequiresToken(t *testing.T) {
	_, err := parseOptions(map[string]any{"url": "wss://example.com/v1/agent-sessions"})
	if err == nil {
		t.Fatal("expected error when token is missing, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error should mention token, got %q", err.Error())
	}
}

func TestParseOptions_RejectsInvalidURLScheme(t *testing.T) {
	for _, bad := range []string{"http://example.com", "https://example.com", "example.com"} {
		_, err := parseOptions(map[string]any{"url": bad, "token": "secret"})
		if err == nil {
			t.Fatalf("expected error for non-ws scheme %q, got nil", bad)
		}
	}
}

func TestParseOptions_AcceptsWSAndWSS(t *testing.T) {
	for _, good := range []string{"ws://example.com/x", "wss://example.com/x"} {
		if _, err := parseOptions(map[string]any{"url": good, "token": "secret"}); err != nil {
			t.Fatalf("expected %q to parse, got %v", good, err)
		}
	}
}

func TestParseOptions_AppliesDefaults(t *testing.T) {
	o, err := parseOptions(map[string]any{"url": "wss://example.com/x", "token": "secret"})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.connectTimeout != 15*time.Second {
		t.Errorf("connectTimeout default = %v, want 15s", o.connectTimeout)
	}
	if o.requestTimeout != 120*time.Second {
		t.Errorf("requestTimeout default = %v, want 120s", o.requestTimeout)
	}
	if o.heartbeatInterval != 30*time.Second {
		t.Errorf("heartbeatInterval default = %v, want 30s", o.heartbeatInterval)
	}
	if !o.resume {
		t.Errorf("resume default = false, want true")
	}
}

func TestParseOptions_RequestTimeoutDistinctFromConnectTimeout(t *testing.T) {
	// §3: request_timeout_secs must not collapse onto the connect budget.
	o, err := parseOptions(map[string]any{"url": "wss://example.com/x", "token": "secret"})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.requestTimeout <= o.connectTimeout {
		t.Errorf("requestTimeout (%v) must exceed connectTimeout (%v) by default", o.requestTimeout, o.connectTimeout)
	}
}

func TestParseOptions_ParsesExplicitValues(t *testing.T) {
	o, err := parseOptions(map[string]any{
		"url":                     "wss://example.com/x",
		"token":                   "secret",
		"project":                 "cc-connect",
		"default_agent":           "codex",
		"connect_timeout_secs":    int64(5),
		"request_timeout_secs":    int64(90),
		"heartbeat_interval_secs": int64(10),
		"resume":                  false,
	})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.project != "cc-connect" || o.defaultAgent != "codex" {
		t.Errorf("project/default_agent not parsed: %+v", o)
	}
	if o.connectTimeout != 5*time.Second || o.requestTimeout != 90*time.Second || o.heartbeatInterval != 10*time.Second {
		t.Errorf("timeouts not parsed: %+v", o)
	}
	if o.resume {
		t.Errorf("resume = true, want false")
	}
}

func TestParseOptions_RejectsNonPositiveTimeouts(t *testing.T) {
	for _, key := range []string{"connect_timeout_secs", "request_timeout_secs", "heartbeat_interval_secs"} {
		_, err := parseOptions(map[string]any{
			"url":   "wss://example.com/x",
			"token": "secret",
			key:     int64(0),
		})
		if err == nil {
			t.Fatalf("expected error for %s = 0, got nil", key)
		}
	}
}

func TestParseOptions_WorkspaceExplicitWins(t *testing.T) {
	o, err := parseOptions(map[string]any{
		"url":       "wss://example.com/x",
		"token":     "secret",
		"workspace": "github.com/ssc806/cc-connect",
		"work_dir":  "/home/me/code",
	})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.workspace != "github.com/ssc806/cc-connect" {
		t.Errorf("workspace = %q, want explicit value", o.workspace)
	}
}

func TestParseOptions_WorkspaceFallsBackToWorkDir(t *testing.T) {
	o, err := parseOptions(map[string]any{
		"url":      "wss://example.com/x",
		"token":    "secret",
		"work_dir": "/home/me/code",
	})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.workspace != "/home/me/code" {
		t.Errorf("workspace = %q, want fallback to work_dir", o.workspace)
	}
}

func TestParseOptions_WorkspaceEmptyWhenNeitherSet(t *testing.T) {
	o, err := parseOptions(map[string]any{"url": "wss://example.com/x", "token": "secret"})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.workspace != "" {
		t.Errorf("workspace = %q, want empty", o.workspace)
	}
}

func TestParseOptions_ResolvesEnvRefToken(t *testing.T) {
	t.Setenv("AGENT_ROUTE_TEST_TOKEN", "resolved-secret")
	o, err := parseOptions(map[string]any{
		"url":   "wss://example.com/x",
		"token": "${AGENT_ROUTE_TEST_TOKEN}",
	})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.token != "resolved-secret" {
		t.Errorf("token = %q, want resolved env value", o.token)
	}
}

func TestErrors_RedactToken(t *testing.T) {
	const token = "super-secret-token-value"
	raw := "dial wss://agent-route.example.com failed with Authorization: Bearer " + token
	got := redactSecrets(raw, token)
	if strings.Contains(got, token) {
		t.Fatalf("redactSecrets leaked the token: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("redactSecrets should mark the redaction, got %q", got)
	}
}

func TestErrors_RedactsSignedURLQuery(t *testing.T) {
	raw := "artifact at https://object-store.example.com/o/report.md?X-Amz-Signature=abc123&exp=999 expired"
	got := redactSecrets(raw, "")
	if strings.Contains(got, "X-Amz-Signature=abc123") {
		t.Fatalf("redactSecrets leaked a signed URL query: %q", got)
	}
}

func TestProtocolError_PreservesCodeAndRetryable(t *testing.T) {
	e := &ProtocolError{Code: "session_busy", Retryable: true, Message: "a run is active"}
	if !strings.Contains(e.Error(), "session_busy") {
		t.Errorf("ProtocolError.Error() should include error_code, got %q", e.Error())
	}
	if !e.Retryable {
		t.Errorf("Retryable not preserved")
	}
}
