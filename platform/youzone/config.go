package youzone

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

func parseConfig(opts map[string]any) (config, error) {
	cfg := config{
		baseURL:            defaultBaseURL,
		apiPrefix:          defaultAPIPrefix,
		robotExplain:       defaultRobotExplain,
		websocketProtocols: []string{"xmpp"},
		heartbeatMode:      heartbeatXMPPWhitespace,
		pingInterval:       defaultPingInterval,
		reconnectDelays:    []time.Duration{time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second},
		httpTimeout:        defaultHTTPTimeout,

		accessTokenHelperTimeout: defaultAccessTokenHelperTimeout,
		accessTokenTTL:           defaultAccessTokenTTL,
		accessTokenRefreshBefore: defaultAccessTokenRefreshBefore,
	}
	if v := optString(opts, "base_url"); v != "" {
		cfg.baseURL = strings.TrimRight(v, "/")
	}
	if _, err := url.ParseRequestURI(cfg.baseURL); err != nil {
		return cfg, fmt.Errorf("youzone: base_url: %w", err)
	}
	if v := optString(opts, "api_prefix"); v != "" {
		cfg.apiPrefix = normalizePrefix(v)
	}
	cfg.robotID = optString(opts, "robot_id")
	cfg.accessToken = optString(opts, "access_token")
	cfg.tenantID = optString(opts, "tenant_id")
	cfg.machineCode = optString(opts, "machine_code")
	cfg.robotExplain = defaultString(optString(opts, "robot_explain"), defaultRobotExplain)
	cfg.allowFrom = optString(opts, "allow_from")
	cfg.autoCreateRobot = optBool(opts, "auto_create_robot")
	cfg.enableTokenHeaderFallback = optBool(opts, "enable_token_header_fallback")
	cfg.logInboundRaw = optBool(opts, "log_inbound_raw")
	if v := optString(opts, "websocket_protocols"); v != "" {
		cfg.websocketProtocols = splitCSV(v)
	}
	if len(cfg.websocketProtocols) == 0 {
		cfg.websocketProtocols = []string{"xmpp"}
	}
	if v := optString(opts, "heartbeat_mode"); v != "" {
		cfg.heartbeatMode = strings.ToLower(strings.TrimSpace(v))
	}
	if cfg.heartbeatMode != heartbeatXMPPWhitespace && cfg.heartbeatMode != heartbeatWSPing {
		return cfg, fmt.Errorf("youzone: heartbeat_mode must be %q or %q", heartbeatXMPPWhitespace, heartbeatWSPing)
	}
	if v := optString(opts, "ping_interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("youzone: ping_interval: %w", err)
		}
		cfg.pingInterval = d
	}
	if cfg.pingInterval < 5*time.Second {
		cfg.pingInterval = 5 * time.Second
	}
	if v := optString(opts, "reconnect_delays"); v != "" {
		delays, err := parseDurationList(v)
		if err != nil {
			return cfg, fmt.Errorf("youzone: reconnect_delays: %w", err)
		}
		cfg.reconnectDelays = delays
	}
	if v := optString(opts, "http_timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("youzone: http_timeout: %w", err)
		}
		cfg.httpTimeout = d
	}
	helper, err := optStringList(opts, "access_token_helper")
	if err != nil {
		return cfg, fmt.Errorf("youzone: access_token_helper: %w", err)
	}
	cfg.accessTokenHelper = helper
	cfg.accessTokenSource = strings.ToLower(optString(opts, "access_token_source"))
	if cfg.accessTokenSource != "" && cfg.accessTokenSource != accessTokenSourceChrome {
		return cfg, fmt.Errorf("youzone: access_token_source must be %q", accessTokenSourceChrome)
	}
	if len(cfg.accessTokenHelper) > 0 && cfg.accessTokenSource != "" {
		return cfg, fmt.Errorf("youzone: access_token_helper and access_token_source are mutually exclusive")
	}
	cfg.chromeProfile = optString(opts, "chrome_profile")
	if cfg.chromeProfile != "" && cfg.accessTokenSource != accessTokenSourceChrome {
		return cfg, fmt.Errorf("youzone: chrome_profile only applies when access_token_source = %q", accessTokenSourceChrome)
	}
	if err := parseHelperDurations(opts, &cfg); err != nil {
		return cfg, err
	}
	if len(cfg.accessTokenHelper) == 0 && cfg.accessTokenSource == "" && cfg.accessToken == "" {
		return cfg, fmt.Errorf("youzone: access_token is required unless access_token_helper or access_token_source is configured")
	}
	if cfg.tenantID == "" {
		return cfg, fmt.Errorf("youzone: tenant_id is required")
	}
	if cfg.robotID == "" && cfg.machineCode == "" {
		return cfg, fmt.Errorf("youzone: robot_id is required unless machine_code is configured")
	}
	if cfg.autoCreateRobot && cfg.machineCode == "" {
		return cfg, fmt.Errorf("youzone: machine_code is required when auto_create_robot is true")
	}
	return cfg, nil
}

func optString(opts map[string]any, key string) string {
	v, _ := opts[key].(string)
	return strings.TrimSpace(v)
}

func optBool(opts map[string]any, key string) bool {
	v, _ := opts[key].(bool)
	return v
}

// optStringList reads an option that may be either a single string (a bare
// executable path) or a TOML array of strings (argv). The string form is
// rejected when it contains whitespace: splitting it on spaces would turn the
// helper option into a shell-injection surface, so a command that needs
// arguments must use the explicit array form instead.
func optStringList(opts map[string]any, key string) ([]string, error) {
	v, ok := opts[key]
	if !ok || v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil, nil
		}
		if strings.ContainsAny(s, " \t\n\r") {
			return nil, fmt.Errorf("string form must be a single executable path with no spaces; use the array form to pass arguments")
		}
		return []string{s}, nil
	case []string:
		return validateArgv(t)
	case []any:
		argv := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("array elements must all be strings")
			}
			argv = append(argv, s)
		}
		return validateArgv(argv)
	default:
		return nil, fmt.Errorf("must be a string or an array of strings")
	}
}

func validateArgv(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("array must not be empty")
	}
	for _, e := range argv {
		if strings.TrimSpace(e) == "" {
			return nil, fmt.Errorf("array must not contain empty elements")
		}
	}
	return argv, nil
}

// parseHelperDurations parses and validates the three access-token timing
// options. Validation fails loudly rather than silently reverting to defaults:
// a non-positive helper timeout leaves a stuck helper holding the refresh lock
// forever, and a refresh-before window that meets or exceeds the TTL puts every
// freshly fetched token straight back into the refresh window.
func parseHelperDurations(opts map[string]any, cfg *config) error {
	for _, d := range []struct {
		key    string
		target *time.Duration
	}{
		{"access_token_helper_timeout", &cfg.accessTokenHelperTimeout},
		{"access_token_ttl", &cfg.accessTokenTTL},
		{"access_token_refresh_before", &cfg.accessTokenRefreshBefore},
	} {
		if v := optString(opts, d.key); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("youzone: %s: %w", d.key, err)
			}
			*d.target = parsed
		}
	}
	if cfg.accessTokenHelperTimeout <= 0 {
		return fmt.Errorf("youzone: access_token_helper_timeout must be > 0")
	}
	if cfg.accessTokenTTL <= 0 {
		return fmt.Errorf("youzone: access_token_ttl must be > 0")
	}
	if cfg.accessTokenRefreshBefore < 0 {
		return fmt.Errorf("youzone: access_token_refresh_before must be >= 0")
	}
	if cfg.accessTokenRefreshBefore >= cfg.accessTokenTTL {
		return fmt.Errorf("youzone: access_token_refresh_before (%s) must be less than access_token_ttl (%s)",
			cfg.accessTokenRefreshBefore, cfg.accessTokenTTL)
	}
	return nil
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

func normalizePrefix(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "/" {
		return ""
	}
	v = "/" + strings.Trim(v, "/")
	return v
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseDurationList(v string) ([]time.Duration, error) {
	parts := splitCSV(v)
	delays := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, err
		}
		if d < 0 {
			d = 0
		}
		delays = append(delays, d)
	}
	if len(delays) == 0 {
		return nil, fmt.Errorf("empty duration list")
	}
	return delays, nil
}
