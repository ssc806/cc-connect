package agentroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// adapterVersion is reported in the recommended X-CC-Connect-Version header
// and the connection.hello client block. It is intentionally coarse — the
// header is optional and only used by the server for diagnostics.
const adapterVersion = "1"

// errClientClosed is the sentinel returned when the caller closed the client
// (as opposed to an unexpected connection loss).
var errClientClosed = errors.New("agentroute: connection closed")

// websocketConn is the minimal websocket surface rpcClient needs. *websocket.Conn
// from gorilla/websocket already satisfies it; tests can substitute a fake.
type websocketConn interface {
	ReadJSON(v any) error
	WriteJSON(v any) error
	SetWriteDeadline(t time.Time) error
	Close() error
}

// rpcClient owns one WebSocket connection and multiplexes JSON-RPC requests,
// responses and server notifications over it. A single reader goroutine
// dispatches frames; writes are serialized by writeMu.
type rpcClient struct {
	conn websocketConn
	opts options

	// writeSem is a cap-1 semaphore serializing writes. Unlike a sync.Mutex it
	// can be acquired with a timeout, so a bounded caller (notably Close()) is
	// never stalled past its budget by a previous write stuck inside WriteJSON.
	writeSem chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	events chan sessionEventNotification

	closeOnce sync.Once
	closed    chan struct{}
	closeMu   sync.Mutex
	closeErr  error
}

// newRPCClient dials the agent-route endpoint, completes the connection.hello
// handshake, and starts the reader + heartbeat goroutines.
//
// The dial and handshake are bounded by opts.connectTimeout; every later call
// is bounded by opts.requestTimeout (see implementation plan §3).
func newRPCClient(ctx context.Context, opts options) (*rpcClient, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+opts.token)
	header.Set("X-AgentRoute-Protocol", protocolHeader)
	header.Set("X-CC-Connect-Version", adapterVersion)
	if opts.project != "" {
		header.Set("X-CC-Connect-Project", opts.project)
	}

	dialer := websocket.Dialer{HandshakeTimeout: opts.connectTimeout}
	dialCtx, cancel := context.WithTimeout(ctx, opts.connectTimeout)
	defer cancel()

	conn, resp, err := dialer.DialContext(dialCtx, opts.url, header)
	if err != nil {
		return nil, dialError(err, resp, opts.token)
	}

	c := &rpcClient{
		conn:     conn,
		opts:     opts,
		pending:  map[string]chan rpcResponse{},
		events:   make(chan sessionEventNotification, 64),
		closed:   make(chan struct{}),
		writeSem: make(chan struct{}, 1),
	}
	go c.readLoop()

	// connection.hello validates auth/protocol after the socket opens; bound
	// it by the connect budget, not the request budget.
	hello := helloParams{
		Protocol: protocolName,
		Version:  protocolVersion,
		Client: helloClient{
			Name:         "cc-connect",
			Version:      adapterVersion,
			AgentAdapter: "agentroute",
		},
		Capabilities: []string{"text", "stream", "permission", "cancel", "session_list"},
	}
	var helloRes helloResult
	if err := c.callWithTimeout(ctx, opts.connectTimeout, methodConnectionHello, hello, &helloRes); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("agentroute: handshake: %w", err)
	}
	// connection.hello is the protocol compatibility gate: a server that
	// negotiates a different protocol or version must be rejected before any
	// heartbeat or session RPC runs against an incompatible peer.
	if helloRes.Protocol != protocolName || helloRes.Version != protocolVersion {
		_ = c.Close()
		return nil, &ProtocolError{
			RPCCode:   http.StatusUpgradeRequired,
			Code:      "protocol_unsupported",
			Retryable: false,
			Message: fmt.Sprintf("agent-route negotiated protocol %q v%d, adapter requires %q v%d",
				helloRes.Protocol, helloRes.Version, protocolName, protocolVersion),
		}
	}

	go c.heartbeat()
	return c, nil
}

// dialError turns a failed WebSocket dial into a clear, non-retryable typed
// error for the handshake rejections the protocol defines, and never lets the
// token leak into the message.
func dialError(err error, resp *http.Response, token string) error {
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return &ProtocolError{
				RPCCode:   http.StatusUnauthorized,
				Code:      "unauthorized",
				Retryable: false,
				Message:   "agent-route rejected the connection (HTTP 401): token missing or invalid",
			}
		case http.StatusUpgradeRequired:
			return &ProtocolError{
				RPCCode:   http.StatusUpgradeRequired,
				Code:      "protocol_unsupported",
				Retryable: false,
				Message:   "agent-route rejected the protocol handshake (HTTP 426): unsupported X-AgentRoute-Protocol",
			}
		default:
			return fmt.Errorf("agentroute: dial failed (HTTP %d): %s",
				resp.StatusCode, redactSecrets(err.Error(), token))
		}
	}
	return fmt.Errorf("agentroute: dial failed: %s", redactSecrets(err.Error(), token))
}

// call issues a JSON-RPC request bounded by the request timeout.
func (c *rpcClient) call(ctx context.Context, method string, params, result any) error {
	return c.callWithTimeout(ctx, c.opts.requestTimeout, method, params, result)
}

// callWithTimeout issues a JSON-RPC request and waits for the correlated
// response, the connection closing, ctx cancellation, or the timeout.
func (c *rpcClient) callWithTimeout(ctx context.Context, timeout time.Duration, method string, params, result any) error {
	select {
	case <-c.closed:
		return c.closedErr()
	default:
	}

	id := newEnvelopeID()
	ch := make(chan rpcResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	// One absolute deadline spans the whole RPC — write-slot wait, the write,
	// and the response wait below — so callWithTimeout(T) is bounded by ~T, not
	// ~2T (≈T to write, then a fresh T waiting for the response). The bounded
	// Close() path (closeRPCTimeout) depends on this end-to-end budget.
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	if err := c.writeJSON(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}, deadline); err != nil {
		// A failed WebSocket write means the connection is unusable. Fail the
		// client now so Alive() flips immediately and the engine recycles the
		// session, instead of leaving it marked alive until the read loop or
		// heartbeat eventually notices.
		c.fail(fmt.Errorf("write %s failed: %w", method, err))
		return fmt.Errorf("agentroute: send %s: %s", method, redactSecrets(err.Error(), c.opts.token))
	}

	var timeoutCh <-chan time.Time
	if !deadline.IsZero() {
		// time.Until is already negative when the write consumed the whole
		// budget; NewTimer then fires immediately, which is the intent.
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return protocolErrorFromRPC(resp.Error)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("agentroute: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-c.closed:
		return c.closedErr()
	case <-ctx.Done():
		return fmt.Errorf("agentroute: %s cancelled: %w", method, ctx.Err())
	case <-timeoutCh:
		return fmt.Errorf("agentroute: %s timed out after %s", method, timeout)
	}
}

// protocolErrorFromRPC converts a JSON-RPC error object into a typed
// ProtocolError, preserving error_code and retryable.
func protocolErrorFromRPC(e *rpcError) error {
	pe := &ProtocolError{RPCCode: e.Code, Message: e.Message}
	if e.Data != nil {
		pe.Code = e.Data.ErrorCode
		pe.Retryable = e.Data.Retryable
	}
	return pe
}

// readLoop is the single reader goroutine. It dispatches responses to waiting
// callers and notifications to the events channel. On any read error it fails
// the client, which unblocks every waiter.
func (c *rpcClient) readLoop() {
	for {
		var in rpcIncoming
		if err := c.conn.ReadJSON(&in); err != nil {
			c.fail(err)
			return
		}
		switch {
		case in.ID != "" && (in.Result != nil || in.Error != nil):
			c.deliver(in.ID, rpcResponse{Result: in.Result, Error: in.Error})
		case in.Method == methodSessionEvent:
			var note sessionEventNotification
			if err := json.Unmarshal(in.Params, &note); err != nil {
				continue
			}
			select {
			case c.events <- note:
			case <-c.closed:
				return
			}
		default:
			// Unknown frame (e.g. a server-initiated ping) — ignore.
		}
	}
}

// deliver routes a correlated response to its waiting caller.
func (c *rpcClient) deliver(id string, resp rpcResponse) {
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	c.pendingMu.Unlock()
	if ok {
		ch <- resp // buffered (cap 1); the waiter may already be gone
	}
}

// heartbeat sends an application-level ping every heartbeatInterval and fails
// the client if a ping does not complete, so a half-dead connection is
// detected promptly rather than on the next user turn.
func (c *rpcClient) heartbeat() {
	if c.opts.heartbeatInterval <= 0 {
		return
	}
	ticker := time.NewTicker(c.opts.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			var res pingResult
			err := c.callWithTimeout(context.Background(), c.opts.requestTimeout,
				methodPing, pingParams{TS: time.Now().UTC().Format(time.RFC3339)}, &res)
			if err != nil {
				c.fail(fmt.Errorf("heartbeat ping failed: %w", err))
				return
			}
		}
	}
}

// writeJSON serializes one frame onto the connection. Acquiring the write slot
// AND the write itself must finish before deadline: a caller — notably the
// bounded Close() path — cannot be stalled past its budget by a previous write
// stuck inside WriteJSON while holding the slot. deadline is absolute and
// shared with the caller's response wait, so the whole RPC stays within one
// budget rather than getting a fresh one per phase. A zero deadline leaves
// both the slot wait and the write unbounded.
func (c *rpcClient) writeJSON(v any, deadline time.Time) error {
	var slotTimeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		slotTimeout = timer.C
	}

	select {
	case c.writeSem <- struct{}{}:
		defer func() { <-c.writeSem }()
	case <-c.closed:
		return errClientClosed
	case <-slotTimeout:
		return errors.New("write blocked: a previous write did not complete before the deadline")
	}

	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.conn.WriteJSON(v)
}

// fail marks the connection lost exactly once, recording the reason and
// unblocking everything that selects on c.closed.
func (c *rpcClient) fail(err error) {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closeErr = err
		c.closeMu.Unlock()
		close(c.closed)
		_ = c.conn.Close()
	})
}

// Close tears down the connection. It is safe to call multiple times.
func (c *rpcClient) Close() error {
	c.fail(errClientClosed)
	return nil
}

// Done is closed when the connection is lost or Close was called.
func (c *rpcClient) Done() <-chan struct{} { return c.closed }

// closeReason returns why the connection ended (after Done fires).
func (c *rpcClient) closeReason() error { return c.closedErr() }

// closedErr reports the connection-loss error for a waiter.
func (c *rpcClient) closedErr() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closeErr != nil && !errors.Is(c.closeErr, errClientClosed) {
		return fmt.Errorf("agentroute: connection lost: %s",
			redactSecrets(c.closeErr.Error(), c.opts.token))
	}
	return errClientClosed
}
