package youzone

import "time"

const (
	defaultBaseURL          = "https://c2.yonyoucloud.com"
	defaultAPIPrefix        = "/yonbip-ec-link"
	defaultHTTPTimeout      = 30 * time.Second
	defaultPingInterval     = 25 * time.Second
	defaultRobotExplain     = "cc-connect"
	defaultReconnectDelays  = "1s,3s,10s,30s"
	heartbeatXMPPWhitespace = "xmpp-whitespace"
	heartbeatWSPing         = "ws-ping"
)

type config struct {
	baseURL                   string
	apiPrefix                 string
	robotID                   string
	accessToken               string
	tenantID                  string
	machineCode               string
	autoCreateRobot           bool
	robotExplain              string
	allowFrom                 string
	websocketProtocols        []string
	heartbeatMode             string
	pingInterval              time.Duration
	reconnectDelays           []time.Duration
	httpTimeout               time.Duration
	enableTokenHeaderFallback bool
	logInboundRaw             bool
}

type robotRecord struct {
	ID          string
	Name        string
	MachineCode string
	RobotUserID string
}

type sendResult struct {
	Success      bool
	Status       int
	BusinessCode *int
	PacketID     string
	ResponseText string
}

type inboundMessage struct {
	MessageID      string
	SenderID       string
	SenderName     string
	ConversationID string
	Text           string
	ContentType    int
	Type           string
	Raw            []byte
}

type replyContext struct {
	robotID        string
	conversationID string
	senderID       string
	messageID      string
}
