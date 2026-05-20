package youzone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

type client struct {
	cfg        config
	tokens     *tokenManager
	httpClient *http.Client
}

func newClient(cfg config, httpClient *http.Client) *client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.httpTimeout}
	}
	if httpClient.Timeout == 0 {
		httpClient.Timeout = defaultHTTPTimeout
	}
	return &client{cfg: cfg, tokens: newTokenManager(cfg), httpClient: httpClient}
}

func (c *client) listRobots(ctx context.Context, machineCode string) ([]robotRecord, error) {
	target := c.buildURL("robot/web/list")
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(machineCode) != "" {
		q := u.Query()
		q.Set("machineCode", strings.TrimSpace(machineCode))
		u.RawQuery = q.Encode()
	}
	finalURL := u.String()
	// listRobots is a standalone GET — it does not go through postJSON, so it
	// needs its own doWithAuthRetry wrap. Without it, a process that starts
	// while the token is already dead would hit the CAS login page during
	// connectLoop -> resolveRobotID -> listRobots and never recover.
	resp, body, err := c.doWithAuthRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
		if err != nil {
			return nil, err
		}
		if err := c.setHeaders(ctx, req, false); err != nil {
			return nil, err
		}
		return req, nil
	}, c.standardAuthFailed)
	if err != nil {
		return nil, fmt.Errorf("youzone: list robots: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("youzone: list robots: HTTP %d: %s", resp.StatusCode, c.redactBody(body))
	}
	return normalizeRobotList(body), nil
}

func (c *client) createRobot(ctx context.Context, machineCode, robotExplain string) (robotRecord, error) {
	machineCode = strings.TrimSpace(machineCode)
	if machineCode == "" {
		return robotRecord{}, fmt.Errorf("youzone: create robot: machineCode is required")
	}
	payload := map[string]string{"machineCode": machineCode}
	if strings.TrimSpace(robotExplain) != "" {
		payload["robotExplain"] = strings.TrimSpace(robotExplain)
	}
	// createRobot goes through postJSON and so inherits its auth-failure retry.
	body, err := c.postJSON(ctx, "robot/web/create", payload)
	if err != nil {
		return robotRecord{}, fmt.Errorf("youzone: create robot: %w", err)
	}
	robot := normalizeRobot(body)
	if robot.ID == "" {
		return robotRecord{}, fmt.Errorf("youzone: create robot: missing robot id in response: %s", c.redactBody(body))
	}
	return robot, nil
}

func (c *client) getWSS(ctx context.Context, robotID string) (string, error) {
	payload := map[string]string{"id": robotID, "robotId": robotID}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	buildReq := func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.buildURL("claw-robot/client/getWss"), bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := c.setHeaders(ctx, req, true); err != nil {
			return nil, err
		}
		return req, nil
	}
	// getWss builds its own request and calls doWithAuthRetry directly rather
	// than going through postJSON: stacking a postJSON retry under a getWss
	// retry would refresh twice for one getWss operation. The endpoint-specific
	// authFailed adds one case to the standard signals — an HTTP 200 /
	// business-code-200 response with no wss URL that still looks like a login
	// context. postJSON cannot detect that "fake success" because it does not
	// know the caller expects a wss field.
	authFailed := func(resp *http.Response, body []byte) bool {
		if c.standardAuthFailed(resp, body) {
			return true
		}
		return normalizeWSS(body) == "" && looksLikeLoginContext(body)
	}
	resp, body, err := c.doWithAuthRetry(ctx, buildReq, authFailed)
	if err != nil {
		return "", fmt.Errorf("youzone: get wss: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("youzone: get wss: HTTP %d: %s", resp.StatusCode, c.redactBody(body))
	}
	if code, ok := businessCode(body); ok && code != 200 {
		return "", fmt.Errorf("youzone: get wss: business code %d: %s", code, c.redactBody(body))
	}
	wss := normalizeWSS(body)
	if wss == "" {
		return "", fmt.Errorf("youzone: get wss: missing wss in response: %s", c.redactBody(body))
	}
	return wss, nil
}

func (c *client) sendMessage(ctx context.Context, robotID string, msg outboundMessage) (sendResult, error) {
	// No conversation/recipient field: a YOUZONE "claw robot" is bound to one
	// conversation, so robotId alone identifies the target — same as YonClaw's
	// claw-robot/client/sendMessage. See outbound.go for the payload rationale.
	payload := map[string]any{
		"id":          robotID,
		"robotId":     robotID,
		"content":     msg.Content,
		"contentType": msg.ContentType,
	}
	if strings.TrimSpace(msg.Extend) != "" {
		payload["extend"] = msg.Extend
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return sendResult{}, err
	}
	// sendMessage may have side effects, but on an auth failure the server has
	// not accepted the message. A single refresh-and-retry is therefore safe;
	// non-auth failures (network errors, etc.) are never retried here.
	resp, body, err := c.doWithAuthRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.buildURL("claw-robot/client/sendMessage"), bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := c.setHeaders(ctx, req, true); err != nil {
			return nil, err
		}
		return req, nil
	}, c.standardAuthFailed)
	if err != nil {
		return sendResult{}, fmt.Errorf("youzone: send message: %w", err)
	}
	result := parseSendResult(resp.StatusCode, body)
	if !result.Success {
		return result, fmt.Errorf("youzone: send message failed: HTTP %d business=%v body=%s", result.Status, result.BusinessCode, c.redactBody(body))
	}
	return result, nil
}

func (c *client) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, body, err := c.doWithAuthRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.buildURL(path), bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := c.setHeaders(ctx, req, true); err != nil {
			return nil, err
		}
		return req, nil
	}, c.standardAuthFailed)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, c.redactBody(body))
	}
	if code, ok := businessCode(body); ok && code != 200 {
		return nil, fmt.Errorf("business code %d: %s", code, c.redactBody(body))
	}
	return body, nil
}

// doWithAuthRetry runs an HTTP request and, when the response indicates the
// access token was rejected, force-refreshes the token and retries the request
// exactly once. buildReq is invoked afresh per attempt so the retry picks up
// the refreshed token and a rewindable request body. Every HTTP-bearing method
// routes through here, so GET and POST share identical refresh-retry behavior.
func (c *client) doWithAuthRetry(
	ctx context.Context,
	buildReq func(context.Context) (*http.Request, error),
	authFailed func(*http.Response, []byte) bool,
) (*http.Response, []byte, error) {
	req, err := buildReq(ctx)
	if err != nil {
		return nil, nil, err
	}
	resp, body, err := c.do(req)
	if err != nil {
		return resp, body, err
	}
	if !authFailed(resp, body) {
		return resp, body, nil
	}

	slog.Warn("youzone: auth failed, refreshing token", "http_status", resp.StatusCode)
	if _, err := c.tokens.Token(ctx, true); err != nil {
		return resp, body, fmt.Errorf("auth failed and token refresh failed: %w", err)
	}

	req2, err := buildReq(ctx)
	if err != nil {
		return nil, nil, err
	}
	resp2, body2, err := c.do(req2)
	if err != nil {
		return resp2, body2, err
	}
	if authFailed(resp2, body2) {
		return resp2, body2, fmt.Errorf("auth failed after token refresh")
	}
	return resp2, body2, nil
}

func (c *client) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

func (c *client) buildURL(path string) string {
	return strings.TrimRight(c.cfg.baseURL, "/") + c.cfg.apiPrefix + "/" + strings.TrimLeft(path, "/")
}

// setHeaders stamps the YOUZONE auth headers onto req. The access token is
// pulled from the token manager (which may lazily run the helper on the first
// request or when the cached token has entered its refresh window) rather than
// from a value frozen at startup.
func (c *client) setHeaders(ctx context.Context, req *http.Request, jsonBody bool) error {
	token, err := c.tokens.Token(ctx, false)
	if err != nil {
		return err
	}
	if jsonBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", fmt.Sprintf("yht_access_token=%s; tenantid=%s", token, c.cfg.tenantID))
	req.Header.Set("Origin", strings.TrimRight(c.cfg.baseURL, "/"))
	req.Header.Set("Referer", strings.TrimRight(c.cfg.baseURL, "/")+"/")
	req.Header.Set("User-Agent", "cc-connect-youzone/0.1")
	if c.cfg.enableTokenHeaderFallback {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Access-Token", token)
	}
	return nil
}

func (c *client) redactBody(body []byte) string {
	text := c.tokens.Redact(string(body))
	if len(text) > 1024 {
		text = text[:1024] + "..."
	}
	return text
}

// strongLoginMarkers are substrings that identify a CAS / SSO login page on
// their own, regardless of which endpoint returned the body.
var strongLoginMarkers = []string{
	"<title>登录",
	"cas/login",
	"casloginform",
	"用户登录",
	"请重新登录",
}

// loginContextMarkers is a looser set used only by getWss, and only when the
// response also lacks a wss URL. On its own a bare "login" substring would be
// too weak to act on, but combined with "this getWss response has no wss" it
// reliably flags a login-redirect payload.
var loginContextMarkers = []string{
	"<title>登录",
	"cas/login",
	"casloginform",
	"用户登录",
	"请重新登录",
	"login",
	"登录",
	"passport",
}

// authExpiredBusinessCodes lists YOUZONE business "code" values that mean the
// session/token expired. It is intentionally empty: no specific code has been
// confirmed against production responses yet (see the plan's open questions).
// CAS login HTML plus HTTP 401/403 cover the observed failure mode; add codes
// here as they are confirmed from real traffic or fixtures.
var authExpiredBusinessCodes = map[int]bool{}

// standardAuthFailed reports whether an HTTP response means the access token
// was rejected: an explicit 401/403, a CAS/SSO login page served instead of
// JSON, or a confirmed auth-expiry business code.
func (c *client) standardAuthFailed(resp *http.Response, body []byte) bool {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return true
	}
	if isLoginPage(body) {
		return true
	}
	if code, ok := businessCode(body); ok && authExpiredBusinessCodes[code] {
		return true
	}
	return false
}

// isLoginPage reports whether a response body is the CAS/SSO login page YOUZONE
// serves when yht_access_token is missing or expired. Detection keys on
// CAS/login markers in the body — not on an HTML content-type alone. A
// transient proxy or gateway failure (502/503/504) is also delivered as an HTML
// error page; classifying that as an auth failure would force a needless token
// refresh and, because sendMessage retries POSTs on an auth failure, risk
// resending a message the server may already have accepted.
func isLoginPage(body []byte) bool {
	return containsAnyFold(body, strongLoginMarkers)
}

// looksLikeLoginContext is the loose login heuristic for getWss. It is only
// ever consulted alongside "the response carries no wss URL", which makes the
// weaker markers safe to act on.
func looksLikeLoginContext(body []byte) bool {
	return containsAnyFold(body, loginContextMarkers)
}

func containsAnyFold(body []byte, markers []string) bool {
	lower := strings.ToLower(string(body))
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func normalizeRobotList(body []byte) []robotRecord {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	candidates := []any{valueAt(v, "data", "dataList"), valueAt(v, "dataList"), valueAt(v, "data")}
	for _, candidate := range candidates {
		if arr, ok := candidate.([]any); ok {
			robots := make([]robotRecord, 0, len(arr))
			for _, item := range arr {
				if robot := readRobot(item); robot.ID != "" {
					robots = append(robots, robot)
				}
			}
			return robots
		}
	}
	return nil
}

func normalizeRobot(body []byte) robotRecord {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return robotRecord{}
	}
	for _, candidate := range []any{valueAt(v, "data"), valueAt(v, "result"), valueAt(v, "robot"), v} {
		if robot := readRobot(candidate); robot.ID != "" {
			return robot
		}
	}
	return robotRecord{}
}

func readRobot(v any) robotRecord {
	m, ok := v.(map[string]any)
	if !ok {
		return robotRecord{}
	}
	return robotRecord{
		ID:          readString(m["id"]),
		Name:        readString(m["name"]),
		MachineCode: readString(m["machineCode"]),
		RobotUserID: readString(m["robotUserId"]),
	}
}

func normalizeWSS(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return pickString(v, "wss", "url", "wsUrl", "wssUrl")
}

func parseSendResult(status int, body []byte) sendResult {
	result := sendResult{Status: status, ResponseText: string(body)}
	var parsed struct {
		Code *int `json:"code"`
		Data struct {
			PacketID string `json:"packetId"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &parsed)
	result.BusinessCode = parsed.Code
	result.PacketID = strings.TrimSpace(parsed.Data.PacketID)
	result.Success = status >= 200 && status < 300 && (parsed.Code == nil || *parsed.Code == 200)
	return result
}

func businessCode(body []byte) (int, bool) {
	var parsed struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Code == nil {
		return 0, false
	}
	return *parsed.Code, true
}

func valueAt(v any, path ...string) any {
	cur := v
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

func pickString(v any, keys ...string) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range keys {
		if s := readString(m[key]); s != "" {
			return s
		}
	}
	if data := pickString(m["data"], keys...); data != "" {
		return data
	}
	return ""
}

func readString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
