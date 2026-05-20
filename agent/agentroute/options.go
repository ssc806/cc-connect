package agentroute

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// options is the parsed, validated adapter configuration. All fields are
// derived from the project's [projects.agent.options] map (see the
// implementation plan §3).
type options struct {
	url               string
	token             string
	project           string
	workspace         string
	defaultAgent      string
	connectTimeout    time.Duration
	requestTimeout    time.Duration
	heartbeatInterval time.Duration
	resume            bool
}

const (
	defaultConnectTimeout    = 15 * time.Second
	defaultRequestTimeout    = 120 * time.Second
	defaultHeartbeatInterval = 30 * time.Second
)

// parseOptions validates and normalizes the raw config map.
//
// connect_timeout_secs bounds only the WebSocket dial + connection.hello
// handshake; request_timeout_secs bounds every other JSON-RPC call. They are
// deliberately independent — the protocol recommends a 120s provisioning wait
// that session.start can legitimately consume.
func parseOptions(raw map[string]any) (options, error) {
	o := options{
		connectTimeout:    defaultConnectTimeout,
		requestTimeout:    defaultRequestTimeout,
		heartbeatInterval: defaultHeartbeatInterval,
		resume:            true,
	}

	o.url = strings.TrimSpace(resolveEnvRef(optString(raw, "url")))
	o.token = strings.TrimSpace(resolveEnvRef(optString(raw, "token")))
	o.project = strings.TrimSpace(optString(raw, "project"))
	o.defaultAgent = strings.TrimSpace(optString(raw, "default_agent"))

	if o.url == "" {
		return options{}, fmt.Errorf("agentroute: option %q is required", "url")
	}
	if o.token == "" {
		return options{}, fmt.Errorf("agentroute: option %q is required", "token")
	}

	u, err := url.Parse(o.url)
	if err != nil {
		return options{}, fmt.Errorf("agentroute: invalid url: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return options{}, fmt.Errorf("agentroute: url scheme must be ws:// or wss://, got %q", u.Scheme)
	}

	// workspace fallback order: explicit workspace > work_dir > empty.
	// Step 2 is what lets a per-workspace cloned agent (engine sets only
	// work_dir) still report a meaningful session.start.workspace.
	o.workspace = strings.TrimSpace(optString(raw, "workspace"))
	if o.workspace == "" {
		o.workspace = strings.TrimSpace(optString(raw, "work_dir"))
	}

	if o.connectTimeout, err = optDuration(raw, "connect_timeout_secs", o.connectTimeout); err != nil {
		return options{}, err
	}
	if o.requestTimeout, err = optDuration(raw, "request_timeout_secs", o.requestTimeout); err != nil {
		return options{}, err
	}
	if o.heartbeatInterval, err = optDuration(raw, "heartbeat_interval_secs", o.heartbeatInterval); err != nil {
		return options{}, err
	}

	if v, ok := raw["resume"]; ok {
		o.resume = asBool(v, true)
	}

	return o, nil
}

// resolveEnvRef expands a "${ENV_NAME}" reference if the cc-connect config
// layer has not already done so. A plain value is returned unchanged.
func resolveEnvRef(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		return os.Getenv(v[2 : len(v)-1])
	}
	return v
}

// optString returns a string option, tolerating any value type.
func optString(raw map[string]any, key string) string {
	v, ok := raw[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// optDuration reads an integer "seconds" option and returns it as a Duration.
// A missing key yields def; a present value must be a positive integer.
func optDuration(raw map[string]any, key string, def time.Duration) (time.Duration, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return def, nil
	}
	n, ok := asInt(v)
	if !ok {
		return 0, fmt.Errorf("agentroute: option %q must be an integer number of seconds", key)
	}
	if n <= 0 {
		return 0, fmt.Errorf("agentroute: option %q must be > 0, got %d", key, n)
	}
	return time.Duration(n) * time.Second, nil
}

// asInt coerces the common numeric types a TOML/JSON decoder may produce.
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	default:
		return 0, false
	}
}

// asBool coerces a bool option, tolerating string forms ("true"/"false").
func asBool(v any, def bool) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}
