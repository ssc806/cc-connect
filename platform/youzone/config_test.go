package youzone

import (
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":     "robot-1",
		"access_token": "token",
		"tenant_id":    "tenant",
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", cfg.baseURL, defaultBaseURL)
	}
	if cfg.apiPrefix != defaultAPIPrefix {
		t.Fatalf("apiPrefix = %q, want %q", cfg.apiPrefix, defaultAPIPrefix)
	}
	if got := cfg.websocketProtocols; len(got) != 1 || got[0] != "xmpp" {
		t.Fatalf("websocketProtocols = %#v, want [xmpp]", got)
	}
	if cfg.heartbeatMode != heartbeatXMPPWhitespace {
		t.Fatalf("heartbeatMode = %q, want %q", cfg.heartbeatMode, heartbeatXMPPWhitespace)
	}
	if cfg.pingInterval != 25*time.Second {
		t.Fatalf("pingInterval = %v, want 25s", cfg.pingInterval)
	}
}

func TestParseConfigRequiresAuth(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"robot_id":  "robot",
		"tenant_id": "tenant",
	})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want missing access_token")
	}
}

func TestParseConfigHelperStringFormAllowsEmptyAccessToken(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":            "robot",
		"tenant_id":           "tenant",
		"access_token_helper": "/opt/cc-connect/get-token.mjs",
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if got := cfg.accessTokenHelper; len(got) != 1 || got[0] != "/opt/cc-connect/get-token.mjs" {
		t.Fatalf("accessTokenHelper = %#v, want single-element [/opt/cc-connect/get-token.mjs]", got)
	}
	if cfg.accessToken != "" {
		t.Fatalf("accessToken = %q, want empty when helper configured", cfg.accessToken)
	}
}

func TestParseConfigChromeTokenSourceAllowsEmptyAccessToken(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":            "robot",
		"tenant_id":           "tenant",
		"access_token_source": "chrome",
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.accessTokenSource != "chrome" {
		t.Fatalf("accessTokenSource = %q, want chrome", cfg.accessTokenSource)
	}
	if cfg.accessToken != "" {
		t.Fatalf("accessToken = %q, want empty when chrome source configured", cfg.accessToken)
	}
}

func TestParseConfigRejectsUnknownTokenSource(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"robot_id":            "robot",
		"tenant_id":           "tenant",
		"access_token_source": "firefox",
	})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want unknown access_token_source rejection")
	}
}

func TestParseConfigRejectsHelperAndTokenSourceTogether(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"robot_id":            "robot",
		"tenant_id":           "tenant",
		"access_token_helper": "/opt/cc-connect/get-token",
		"access_token_source": "chrome",
	})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want helper/source conflict rejection")
	}
}

func TestParseConfigHelperArrayForm(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":  "robot",
		"tenant_id": "tenant",
		"access_token_helper": []any{
			"/usr/bin/env", "node", "/opt/cc-connect/get token.mjs",
		},
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	want := []string{"/usr/bin/env", "node", "/opt/cc-connect/get token.mjs"}
	if got := cfg.accessTokenHelper; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("accessTokenHelper = %#v, want %#v", got, want)
	}
}

func TestParseConfigHelperDefaultDurations(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":            "robot",
		"tenant_id":           "tenant",
		"access_token_helper": "/opt/cc-connect/get-token.mjs",
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.accessTokenHelperTimeout != 10*time.Second {
		t.Errorf("accessTokenHelperTimeout = %v, want 10s", cfg.accessTokenHelperTimeout)
	}
	if cfg.accessTokenTTL != 14*time.Hour+30*time.Minute {
		t.Errorf("accessTokenTTL = %v, want 14h30m", cfg.accessTokenTTL)
	}
	if cfg.accessTokenRefreshBefore != 30*time.Minute {
		t.Errorf("accessTokenRefreshBefore = %v, want 30m", cfg.accessTokenRefreshBefore)
	}
}

func TestParseConfigHelperCustomDurations(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":                    "robot",
		"tenant_id":                   "tenant",
		"access_token_helper":         "/opt/cc-connect/get-token.mjs",
		"access_token_helper_timeout": "5s",
		"access_token_ttl":            "2h",
		"access_token_refresh_before": "15m",
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.accessTokenHelperTimeout != 5*time.Second {
		t.Errorf("accessTokenHelperTimeout = %v, want 5s", cfg.accessTokenHelperTimeout)
	}
	if cfg.accessTokenTTL != 2*time.Hour {
		t.Errorf("accessTokenTTL = %v, want 2h", cfg.accessTokenTTL)
	}
	if cfg.accessTokenRefreshBefore != 15*time.Minute {
		t.Errorf("accessTokenRefreshBefore = %v, want 15m", cfg.accessTokenRefreshBefore)
	}
}

func TestParseConfigHelperRejectsBadDurations(t *testing.T) {
	cases := map[string]map[string]any{
		"zero timeout":        {"access_token_helper_timeout": "0s"},
		"negative timeout":    {"access_token_helper_timeout": "-1s"},
		"zero ttl":            {"access_token_ttl": "0s"},
		"negative refresh":    {"access_token_refresh_before": "-1m"},
		"refresh equals ttl":  {"access_token_ttl": "30m", "access_token_refresh_before": "30m"},
		"refresh exceeds ttl": {"access_token_ttl": "30m", "access_token_refresh_before": "1h"},
		"unparseable timeout": {"access_token_helper_timeout": "soon"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			opts := map[string]any{
				"robot_id":            "robot",
				"tenant_id":           "tenant",
				"access_token_helper": "/opt/cc-connect/get-token.mjs",
			}
			for k, v := range extra {
				opts[k] = v
			}
			if _, err := parseConfig(opts); err == nil {
				t.Fatalf("parseConfig() error = nil, want rejection for %s", name)
			}
		})
	}
}

func TestParseConfigHelperStringFormRejectsSpaces(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"robot_id":            "robot",
		"tenant_id":           "tenant",
		"access_token_helper": "/usr/bin/env node /opt/get-token.mjs",
	})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want rejection of space-containing helper string (shell-injection guard)")
	}
}

func TestParseConfigHelperArrayRejectsBadElement(t *testing.T) {
	cases := map[string][]any{
		"empty element": {"/usr/bin/env", ""},
		"non-string":    {"/usr/bin/env", 42},
		"empty array":   {},
	}
	for name, helper := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig(map[string]any{
				"robot_id":            "robot",
				"tenant_id":           "tenant",
				"access_token_helper": helper,
			})
			if err == nil {
				t.Fatalf("parseConfig() error = nil, want rejection for %s", name)
			}
		})
	}
}

func TestParseConfigStaticTokenStillRequiredWithoutHelper(t *testing.T) {
	// Regression: without a helper, access_token remains mandatory.
	_, err := parseConfig(map[string]any{
		"robot_id":  "robot",
		"tenant_id": "tenant",
	})
	if err == nil {
		t.Fatal("parseConfig() error = nil, want missing access_token when no helper configured")
	}
}

func TestParseConfigParsesListsAndDurations(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"robot_id":                     "robot",
		"access_token":                 "token",
		"tenant_id":                    "tenant",
		"websocket_protocols":          "xmpp, custom",
		"reconnect_delays":             "500ms,2s",
		"ping_interval":                "10s",
		"heartbeat_mode":               "ws-ping",
		"auto_create_robot":            true,
		"machine_code":                 "machine",
		"robot_explain":                "explain",
		"enable_token_header_fallback": true,
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if got := cfg.websocketProtocols; len(got) != 2 || got[0] != "xmpp" || got[1] != "custom" {
		t.Fatalf("websocketProtocols = %#v", got)
	}
	if got := cfg.reconnectDelays; len(got) != 2 || got[0] != 500*time.Millisecond || got[1] != 2*time.Second {
		t.Fatalf("reconnectDelays = %#v", got)
	}
	if cfg.heartbeatMode != heartbeatWSPing {
		t.Fatalf("heartbeatMode = %q", cfg.heartbeatMode)
	}
	if !cfg.autoCreateRobot || cfg.machineCode != "machine" || cfg.robotExplain != "explain" {
		t.Fatalf("robot discovery config not parsed: %#v", cfg)
	}
	if !cfg.enableTokenHeaderFallback {
		t.Fatal("enableTokenHeaderFallback = false, want true")
	}
}
