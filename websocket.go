package bonya

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The edge's bonya.v1 WebSocket protocol: binary messages only, one type
// byte then the payload, control payloads are JSON, at most 64 KiB of
// payload per message. The client sends START first; after a failure the
// edge sends ERROR, then closes.
const (
	wsProtocol         = "bonya.v1"
	wsMaxPayload       = 64 << 10
	wsReadWait         = 30 * time.Second // the edge pings every 10 s
	wsWriteWait        = 10 * time.Second
	wsHandshakeTimeout = 60 * time.Second // covers the edge's hold during a wake (30 s)
)

// Frame types.
const (
	frameStart    byte = 0x01
	frameStdin    byte = 0x02
	frameResize   byte = 0x03
	frameSignal   byte = 0x04
	frameDetach   byte = 0x05
	frameStdinEOF byte = 0x06
	frameCancel   byte = 0x07

	frameAccepted      byte = 0x11
	frameOutput        byte = 0x12
	frameExit          byte = 0x13
	frameEnded         byte = 0x14
	frameOutputDropped byte = 0x15
	frameError         byte = 0x16
	frameStderr        byte = 0x17
)

// Close codes the edge uses.
const (
	closeServiceRestart = 1012 // forced pause
	closeGoingAway      = 1001 // edge shutdown
	closeBadRequest     = 4400
	closeAuthFailed     = 4401
	closeNotFound       = 4404
	closeStartTimeout   = 4408
	closeConflict       = 4409
	closeTooLarge       = 4413
	closeLimited        = 4429
	closeUnavailable    = 4503
)

// Problem codes that only appear in stream ERROR frames.
const (
	codeSessionPaused = "session_paused"
	codeEdgeShutdown  = "edge_shutdown"
)

// wsConn is one bonya.v1 connection. Writes are serialized; reads happen in
// one goroutine.
type wsConn struct {
	conn      *websocket.Conn
	sandboxID string
	writeMu   sync.Mutex
	closeOnce sync.Once
}

// dialEdge opens a bonya.v1 WebSocket to path on the edge. It authenticates
// with the sandbox capability when there is one. A refused capability is
// refreshed once and the dial repeated, and transient refusals are retried:
// every failure here happens before the upgrade and the START frame, so
// nothing has run yet.
func (s *Sandbox) dialEdge(ctx context.Context, path, idempotencyKey string) (*wsConn, error) {
	refreshed := false
	for attempt := 0; ; attempt++ {
		token, isCapability := s.edgeBearer(ctx)
		conn, err := s.dialOnce(ctx, path, token, isCapability, idempotencyKey)
		if err == nil {
			return conn, nil
		}
		var e *Error
		if !errors.As(err, &e) {
			return nil, err
		}
		if isCapability && !refreshed && capabilityRefused(err) {
			refreshed = true
			s.cap.invalidate(token)
			attempt--
			continue
		}
		if attempt >= s.client.maxRetries || !transient(e) || !sleepBeforeRetry(ctx, e.RetryAfter, attempt) {
			return nil, err
		}
	}
}

func (s *Sandbox) dialOnce(ctx context.Context, path, token string, isCapability bool, idempotencyKey string) (*wsConn, error) {
	base := s.client.edgeURL
	u := *base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	unescaped, err := url.PathUnescape(path)
	if err != nil {
		return nil, invalidArgument("request path is not valid: " + err.Error())
	}
	u.Path = base.Path + unescaped
	u.RawPath = base.EscapedPath() + path

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("User-Agent", userAgent)
	// A capability is bound to its sandbox; the organization header is sent
	// only with the client credential.
	if !isCapability && s.client.organizationID != "" {
		header.Set(organizationHeader, s.client.organizationID)
	}
	if idempotencyKey != "" {
		header.Set(idempotencyHeader, idempotencyKey)
	}

	conn, resp, err := s.client.wsDialer().DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSwitchingProtocols {
				return nil, responseError(resp, s.ID)
			}
		}
		e := &Error{Kind: ErrUnavailable, Message: "could not open the stream: " + err.Error(), SandboxID: s.ID, cause: err}
		if ctxErr := ctx.Err(); ctxErr != nil {
			e.cause = ctxErr
		}
		return nil, e
	}
	if conn.Subprotocol() != wsProtocol {
		_ = conn.Close()
		return nil, &Error{Kind: ErrInternal, Message: "the edge did not accept the bonya.v1 protocol", SandboxID: s.ID}
	}
	c := &wsConn{conn: conn, sandboxID: s.ID}
	conn.SetReadLimit(wsMaxPayload + 1)
	_ = conn.SetReadDeadline(time.Now().Add(wsReadWait))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadWait))
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(wsWriteWait))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		var netErr interface{ Temporary() bool }
		if errors.As(err, &netErr) && netErr.Temporary() {
			return nil
		}
		return err
	})
	return c, nil
}

// wsDialer reuses the HTTP client's TLS, proxy and dial settings.
func (c *Client) wsDialer() *websocket.Dialer {
	d := &websocket.Dialer{
		Subprotocols:     []string{wsProtocol},
		HandshakeTimeout: wsHandshakeTimeout,
		Proxy:            http.ProxyFromEnvironment,
	}
	if t, ok := c.http.Transport.(*http.Transport); ok && t != nil {
		d.Proxy = t.Proxy
		d.NetDialContext = t.DialContext
		if t.TLSClientConfig != nil {
			d.TLSClientConfig = t.TLSClientConfig.Clone()
		}
	}
	return d
}

// send writes one frame.
func (c *wsConn) send(typeCode byte, payload []byte) error {
	message := make([]byte, 1, len(payload)+1)
	message[0] = typeCode
	message = append(message, payload...)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	return c.conn.WriteMessage(websocket.BinaryMessage, message)
}

// sendData writes data as frames of at most 64 KiB.
func (c *wsConn) sendData(typeCode byte, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), wsMaxPayload)
		if err := c.send(typeCode, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (c *wsConn) sendJSON(typeCode byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.send(typeCode, payload)
}

// recv returns the next frame. Any frame extends the read deadline.
func (c *wsConn) recv() (byte, []byte, error) {
	messageType, message, err := c.conn.ReadMessage()
	if err != nil {
		return 0, nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(wsReadWait))
	if messageType != websocket.BinaryMessage || len(message) == 0 {
		return 0, nil, &Error{Kind: ErrInternal, Message: "the edge sent a frame that is not bonya.v1", SandboxID: c.sandboxID}
	}
	return message[0], message[1:], nil
}

// closeNormal sends a normal close and drops the connection.
func (c *wsConn) closeNormal() {
	c.closeOnce.Do(func() {
		_ = c.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = c.conn.Close()
	})
}

// wsErrorFrame is the payload of an ERROR frame.
type wsErrorFrame struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseErrorFrame(payload []byte) *wsErrorFrame {
	var f wsErrorFrame
	if json.Unmarshal(payload, &f) != nil {
		return &wsErrorFrame{Code: "internal", Message: "the edge sent an unreadable error"}
	}
	return &f
}

// streamKind is what a stream carries; it decides how an interrupted stream
// is reported.
type streamKind int

const (
	streamExec streamKind = iota
	streamAttach
)

// streamFailure turns how a stream ended without a final frame into an
// *Error. It prefers the ERROR frame's code; otherwise the close code.
//
// An interruption (forced pause, edge restart, connection lost, the
// sandbox unreachable) means different things per stream: an exec's command
// may still be running or have finished, so it is ErrOutcomeUnknown; a
// terminal session survives it, so an attach is ErrUnavailable (reattach).
func streamFailure(sandboxID string, frame *wsErrorFrame, readErr error, kind streamKind) *Error {
	e := &Error{SandboxID: sandboxID, cause: readErr}
	closeCode := 0
	var closeErr *websocket.CloseError
	if errors.As(readErr, &closeErr) {
		closeCode = closeErr.Code
		e.Message = closeErr.Text
	}
	if frame != nil {
		e.Code = frame.Code
		if frame.Message != "" {
			e.Message = frame.Message
		}
	}
	interrupted := false
	switch {
	case e.Code == codeSessionPaused || (e.Code == "" && closeCode == closeServiceRestart):
		e.Code = codeSessionPaused
		e.Message = "the sandbox was paused and the stream closed"
		interrupted = true
	case e.Code == codeEdgeShutdown || (e.Code == "" && closeCode == closeGoingAway):
		e.Code = codeEdgeShutdown
		e.Message = "the edge restarted and the stream closed"
		interrupted = true
	case e.Code != "":
		if k, ok := codeKinds[e.Code]; ok {
			e.Kind = k
		} else {
			e.Kind = kindForClose(closeCode)
		}
		interrupted = e.Kind == ErrUnavailable || e.Kind == ErrInternal
	case closeCode >= 4000:
		e.Kind = kindForClose(closeCode)
		interrupted = e.Kind == ErrUnavailable
	default:
		if e.Message == "" {
			e.Message = "the stream connection was lost"
			if readErr != nil {
				e.Message += ": " + readErr.Error()
			}
		}
		interrupted = true
	}
	if !interrupted {
		return e
	}
	switch kind {
	case streamExec:
		e.Kind = ErrOutcomeUnknown
		e.Message += "; the command may still be running or have finished. Run it again with the same IdempotencyKey to get its output and exit status"
	default:
		e.Kind = ErrUnavailable
		e.Message += "; the session keeps running, attach again"
	}
	return e
}

// kindForClose classifies a bonya.v1 close code (4xxx echo REST statuses).
func kindForClose(code int) error {
	switch code {
	case closeBadRequest, closeStartTimeout:
		return ErrInvalidRequest
	case closeAuthFailed:
		return ErrUnauthenticated
	case closeNotFound:
		return ErrNotFound
	case closeConflict:
		return ErrConflict
	case closeTooLarge:
		return ErrTooLarge
	case closeLimited:
		return ErrRateLimited
	case closeUnavailable:
		return ErrUnavailable
	default:
		return ErrUnavailable
	}
}

// ExitStatus is how a command or session process ended.
type ExitStatus struct {
	// ExitCode is the process exit code (meaningful when Signaled is false).
	ExitCode int
	// Signaled reports that a signal ended the process.
	Signaled bool
	// Signal is the signal number when Signaled is true.
	Signal int
}

type exitWire struct {
	ExitCode int32 `json:"exit_code"`
	Signaled bool  `json:"signaled"`
	Signal   int32 `json:"signal"`
}

func (w exitWire) status() *ExitStatus {
	return &ExitStatus{ExitCode: int(w.ExitCode), Signaled: w.Signaled, Signal: int(w.Signal)}
}

func parseExit(payload []byte, sandboxID string) (*ExitStatus, error) {
	var w exitWire
	if err := json.Unmarshal(payload, &w); err != nil {
		return nil, &Error{Kind: ErrInternal, Message: "decode exit frame: " + err.Error(), SandboxID: sandboxID, cause: err}
	}
	return w.status(), nil
}

// resizeWire is the RESIZE payload.
type resizeWire struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

// maxTTYDimension is the edge's limit for cols and rows.
const maxTTYDimension = 512

func checkDimensions(cols, rows int, required bool) error {
	if cols < 0 || rows < 0 || cols > maxTTYDimension || rows > maxTTYDimension {
		return invalidArgument("cols and rows must be between 0 and 512")
	}
	if required && (cols == 0 || rows == 0) {
		return invalidArgument("cols and rows must both be between 1 and 512")
	}
	return nil
}
