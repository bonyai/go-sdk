package bonya

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// attachReconnectRounds bounds how often an attachment reconnects by
	// itself after one interruption (each dial also retries and may be held
	// by the edge while the sandbox wakes).
	attachReconnectRounds = 5
	// attachDetachWait is how long Detach waits for the edge to confirm.
	attachDetachWait = 5 * time.Second
)

// SessionStatus is the state of a terminal session.
type SessionStatus string

const (
	SessionStarting SessionStatus = "starting"
	SessionIdle     SessionStatus = "idle"
	SessionAttached SessionStatus = "attached"
	SessionExited   SessionStatus = "exited"
	SessionKilled   SessionStatus = "killed"
	SessionFailed   SessionStatus = "failed"
)

// Session is a named terminal session in a sandbox. It keeps running when
// nobody is attached and survives a warm pause.
type Session struct {
	Name           string
	Command        []string
	WorkingDir     string
	Status         SessionStatus
	Attached       bool
	StartedAt      time.Time
	LastActivityAt time.Time
	// EndedAt and Exit are set once the session has ended.
	EndedAt *time.Time
	Exit    *ExitStatus
}

type sessionWire struct {
	Name                  string    `json:"name"`
	Command               []string  `json:"command"`
	WorkingDir            string    `json:"working_dir"`
	Status                string    `json:"status"`
	Attached              bool      `json:"attached"`
	StartedAtUnixNanos    int64     `json:"started_at_unix_nanos"`
	LastActivityUnixNanos int64     `json:"last_activity_unix_nanos"`
	EndedAtUnixNanos      int64     `json:"ended_at_unix_nanos"`
	Exit                  *exitWire `json:"exit"`
}

func (w sessionWire) session() Session {
	s := Session{
		Name:           w.Name,
		Command:        w.Command,
		WorkingDir:     w.WorkingDir,
		Status:         SessionStatus(w.Status),
		Attached:       w.Attached,
		StartedAt:      unixNanos(w.StartedAtUnixNanos),
		LastActivityAt: unixNanos(w.LastActivityUnixNanos),
	}
	if w.EndedAtUnixNanos != 0 {
		t := time.Unix(0, w.EndedAtUnixNanos)
		s.EndedAt = &t
	}
	if w.Exit != nil {
		s.Exit = w.Exit.status()
	}
	return s
}

func unixNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func sessionPath(sandboxID, name string, parts ...string) string {
	return sandboxPath(sandboxID, append([]string{"sessions", url.PathEscape(name)}, parts...)...)
}

// SessionOptions describes a terminal session to create.
type SessionOptions struct {
	// Name identifies the session: a lowercase letter, then up to 31
	// lowercase letters, digits or dashes. Required.
	Name string
	// Command is the program and its arguments. Required.
	Command []string
	// Env adds environment variables.
	Env map[string]string
	// WorkingDir is the directory the command starts in.
	WorkingDir string
	// Cols and Rows set the initial terminal size (each at most 512).
	Cols int
	Rows int
	// Replace ends a session of the same name first instead of failing.
	Replace bool
}

type createSessionWire struct {
	Name       string            `json:"name"`
	Command    []string          `json:"command"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Cols       uint32            `json:"cols,omitempty"`
	Rows       uint32            `json:"rows,omitempty"`
	Replace    bool              `json:"replace,omitempty"`
}

// CreateSession starts a terminal session. ErrConflict means a session with
// that name exists (possibly started by an earlier attempt of this call
// whose answer was lost).
func (s *Sandbox) CreateSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	if opts.Name == "" {
		return nil, invalidArgument("session name is required")
	}
	if len(opts.Command) == 0 || opts.Command[0] == "" {
		return nil, invalidArgument("command is required")
	}
	if err := checkDimensions(opts.Cols, opts.Rows, false); err != nil {
		return nil, err
	}
	var out struct {
		Session sessionWire `json:"session"`
	}
	err := s.edgeJSON(ctx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "sessions"),
		json: createSessionWire{
			Name: opts.Name, Command: opts.Command, Env: opts.Env, WorkingDir: opts.WorkingDir,
			Cols: uint32(opts.Cols), Rows: uint32(opts.Rows), Replace: opts.Replace,
		},
		idempotencyKey: newIdempotencyKey(),
	}, &out)
	if err != nil {
		return nil, err
	}
	session := out.Session.session()
	return &session, nil
}

// ListSessions returns the sandbox's terminal sessions.
func (s *Sandbox) ListSessions(ctx context.Context) ([]Session, error) {
	var out struct {
		Sessions []sessionWire `json:"sessions"`
	}
	if err := s.edgeJSON(ctx, &request{method: http.MethodGet, path: sandboxPath(s.ID, "sessions")}, &out); err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(out.Sessions))
	for _, w := range out.Sessions {
		sessions = append(sessions, w.session())
	}
	return sessions, nil
}

// KillSessionOptions tunes KillSession.
type KillSessionOptions struct {
	// Signal is sent first, by name (for example "TERM", "INT"); default TERM.
	Signal string
	// Grace is how long to wait before killing the process group; default 5 s.
	Grace time.Duration
}

// KillSession ends a terminal session and returns its final state.
func (s *Sandbox) KillSession(ctx context.Context, name string, opts KillSessionOptions) (*Session, error) {
	if name == "" {
		return nil, invalidArgument("session name is required")
	}
	if opts.Grace < 0 || opts.Grace.Milliseconds() > int64(^uint32(0)) {
		return nil, invalidArgument("grace is out of range")
	}
	var out struct {
		Session sessionWire `json:"session"`
	}
	err := s.edgeJSON(ctx, &request{
		method: http.MethodPost,
		path:   sessionPath(s.ID, name, "kill"),
		json: struct {
			Signal  string `json:"signal,omitempty"`
			GraceMS uint32 `json:"grace_ms,omitempty"`
		}{opts.Signal, uint32(opts.Grace.Milliseconds())},
		idempotencyKey: newIdempotencyKey(),
	}, &out)
	if err != nil {
		return nil, err
	}
	session := out.Session.session()
	return &session, nil
}

// AttachOptions tunes AttachSession.
type AttachOptions struct {
	// Cols and Rows resize the terminal on attach (each at most 512; zero
	// keeps the current size).
	Cols int
	Rows int
	// MaxReplayBytes asks for up to this much recent output first.
	MaxReplayBytes int
	// Output receives terminal output. Nil discards it.
	Output io.Writer
	// OnOutputDropped is called when the session dropped output because
	// this client read too slowly.
	OnOutputDropped func(droppedBytes int64)
	// NoReconnect turns off reconnecting after a pause, an edge restart or a
	// lost connection. By default the attachment reconnects by itself
	// (without replay, so no output is repeated).
	NoReconnect bool
}

// AttachResult is how an attachment ended.
type AttachResult struct {
	// Exit is set when the session's process exited.
	Exit *ExitStatus
	// Detached reports that this client detached (Detach, Close, or ctx
	// done); the session keeps running.
	Detached bool
	// TakenOver reports that another client attached to the session and
	// this attachment lost it; the session keeps running.
	TakenOver bool
}

// Attachment is a live terminal connection to a session. Output goes to
// AttachOptions.Output; Write sends input. Detach or Close leave the session
// running; KillSession ends it.
type Attachment struct {
	// Session is the session as it was when attached.
	Session Session
	// ReplayedBytes is how much recent output was sent first.
	ReplayedBytes int
	// HistoryDropped reports that older output was no longer kept.
	HistoryDropped bool
	// Cols and Rows are the terminal size after attaching.
	Cols int
	Rows int

	sandbox *Sandbox
	name    string
	output  io.Writer
	dropped func(int64)
	noRetry bool

	mu        sync.Mutex
	ws        *wsConn
	cols      int
	rows      int
	detaching bool

	done      chan struct{}
	result    *AttachResult
	err       error
	stopWatch func() bool
	cancel    context.CancelFunc
}

type attachStartWire struct {
	Cols           uint32 `json:"cols,omitempty"`
	Rows           uint32 `json:"rows,omitempty"`
	MaxReplayBytes uint32 `json:"max_replay_bytes,omitempty"`
}

type attachAcceptedWire struct {
	Session        sessionWire `json:"session"`
	ReplayedBytes  uint32      `json:"replayed_bytes"`
	HistoryDropped bool        `json:"history_dropped"`
	Cols           uint32      `json:"cols"`
	Rows           uint32      `json:"rows"`
}

// AttachSession attaches to a terminal session and returns once the session
// accepted the attach. Cancelling ctx detaches. An attached client takes
// the session over from any earlier one.
func (s *Sandbox) AttachSession(ctx context.Context, name string, opts AttachOptions) (*Attachment, error) {
	if name == "" {
		return nil, invalidArgument("session name is required")
	}
	if err := checkDimensions(opts.Cols, opts.Rows, false); err != nil {
		return nil, err
	}
	if opts.MaxReplayBytes < 0 || int64(opts.MaxReplayBytes) > int64(^uint32(0)) {
		return nil, invalidArgument("max replay bytes is out of range")
	}
	a := &Attachment{
		sandbox: s,
		name:    name,
		output:  orDiscard(opts.Output),
		dropped: opts.OnOutputDropped,
		noRetry: opts.NoReconnect,
		cols:    opts.Cols,
		rows:    opts.Rows,
		done:    make(chan struct{}),
	}
	ws, accepted, err := a.connect(ctx, opts.MaxReplayBytes)
	if err != nil {
		return nil, err
	}
	a.ws = ws
	a.Session = accepted.Session.session()
	a.ReplayedBytes = int(accepted.ReplayedBytes)
	a.HistoryDropped = accepted.HistoryDropped
	a.Cols, a.Rows = int(accepted.Cols), int(accepted.Rows)
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.stopWatch = context.AfterFunc(ctx, func() { _ = a.Close() })
	go a.run(runCtx)
	return a, nil
}

// connect dials, sends START and waits for ACCEPTED. A terminal frame
// before ACCEPTED is not expected; ERROR then close is reported.
func (a *Attachment) connect(ctx context.Context, replay int) (*wsConn, *attachAcceptedWire, error) {
	s := a.sandbox
	ws, err := s.dialEdge(ctx, sessionPath(s.ID, a.name, "attach"), "")
	if err != nil {
		return nil, nil, err
	}
	a.mu.Lock()
	cols, rows := a.cols, a.rows
	a.mu.Unlock()
	start := attachStartWire{Cols: uint32(cols), Rows: uint32(rows), MaxReplayBytes: uint32(replay)}
	if err := ws.sendJSON(frameStart, start); err != nil {
		ws.closeNormal()
		return nil, nil, &Error{Kind: ErrUnavailable, Message: "send START: " + err.Error(), SandboxID: s.ID, cause: err}
	}
	stop := context.AfterFunc(ctx, ws.closeNormal)
	defer stop()
	var errFrame *wsErrorFrame
	for {
		typeCode, payload, err := ws.recv()
		if err != nil {
			ws.closeNormal()
			if ctx.Err() != nil {
				return nil, nil, &Error{Kind: ErrUnavailable, Message: "attach cancelled", SandboxID: s.ID, cause: ctx.Err()}
			}
			var sdkErr *Error
			if errFrame == nil && errors.As(err, &sdkErr) {
				return nil, nil, sdkErr
			}
			return nil, nil, streamFailure(s.ID, errFrame, err, streamAttach)
		}
		switch typeCode {
		case frameAccepted:
			var accepted attachAcceptedWire
			if err := json.Unmarshal(payload, &accepted); err != nil {
				ws.closeNormal()
				return nil, nil, &Error{Kind: ErrInternal, Message: "decode ACCEPTED: " + err.Error(), SandboxID: s.ID, cause: err}
			}
			return ws, &accepted, nil
		case frameError:
			errFrame = parseErrorFrame(payload)
		}
	}
}

// run pumps the current connection and reconnects after an interruption.
func (a *Attachment) run(ctx context.Context) {
	for {
		a.mu.Lock()
		ws := a.ws
		a.mu.Unlock()
		result, err := a.pump(ws)
		if result != nil || a.isDetaching() || a.noRetry || !reconnectable(err) {
			a.finish(result, err)
			return
		}
		ws, err = a.reconnect(ctx)
		if err != nil && !a.isDetaching() {
			a.finish(nil, err)
			return
		}
		a.mu.Lock()
		if ws == nil || a.detaching {
			a.mu.Unlock()
			if ws != nil {
				ws.closeNormal()
			}
			a.finish(&AttachResult{Detached: true}, nil)
			return
		}
		a.ws = ws
		a.mu.Unlock()
	}
}

// reconnectable: the session survives these; another attach may succeed.
func reconnectable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == ErrUnavailable
}

func (a *Attachment) reconnect(ctx context.Context) (*wsConn, error) {
	var lastErr error
	for round := 0; round < attachReconnectRounds; round++ {
		if round > 0 && !sleepBeforeRetry(ctx, 0, round) {
			break
		}
		if a.isDetaching() || ctx.Err() != nil {
			break
		}
		// No replay: this client has seen the output before the break.
		ws, _, err := a.connect(ctx, 0)
		if err == nil {
			return ws, nil
		}
		lastErr = err
		var e *Error
		if !errors.As(err, &e) || !transient(e) {
			return nil, err
		}
	}
	if a.isDetaching() || ctx.Err() != nil {
		return nil, nil
	}
	return nil, lastErr
}

// pump relays one connection until it ends. It returns a result for EXIT,
// ENDED, or a detach by this client.
func (a *Attachment) pump(ws *wsConn) (*AttachResult, error) {
	var errFrame *wsErrorFrame
	for {
		typeCode, payload, err := ws.recv()
		if err != nil {
			ws.closeNormal()
			if a.isDetaching() {
				return &AttachResult{Detached: true}, nil
			}
			var sdkErr *Error
			if errFrame == nil && errors.As(err, &sdkErr) {
				return nil, sdkErr
			}
			return nil, streamFailure(a.sandbox.ID, errFrame, err, streamAttach)
		}
		switch typeCode {
		case frameOutput:
			// A failing writer must not stall the session; output is dropped.
			_, _ = a.output.Write(payload)
		case frameOutputDropped:
			var dropped struct {
				DroppedBytes int64 `json:"dropped_bytes"`
			}
			if json.Unmarshal(payload, &dropped) == nil && a.dropped != nil {
				a.dropped(dropped.DroppedBytes)
			}
		case frameExit:
			exit, perr := parseExit(payload, a.sandbox.ID)
			ws.closeNormal()
			if perr != nil {
				return nil, perr
			}
			return &AttachResult{Exit: exit}, nil
		case frameEnded:
			var ended struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(payload, &ended)
			ws.closeNormal()
			if strings.EqualFold(ended.Reason, "TAKEOVER") {
				return &AttachResult{TakenOver: true}, nil
			}
			return &AttachResult{Detached: true}, nil
		case frameError:
			errFrame = parseErrorFrame(payload)
		default:
			// Frame types this SDK version does not know are skipped.
		}
	}
}

func (a *Attachment) isDetaching() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.detaching
}

func (a *Attachment) finish(result *AttachResult, err error) {
	if result == nil && err == nil {
		result = &AttachResult{Detached: true}
	}
	a.mu.Lock()
	a.result, a.err = result, err
	a.mu.Unlock()
	close(a.done)
	if a.stopWatch != nil {
		a.stopWatch()
	}
	a.cancel()
}

func (a *Attachment) current() (*wsConn, error) {
	select {
	case <-a.done:
		return nil, invalidArgument("the attachment has ended")
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.detaching {
		return nil, invalidArgument("the attachment is detaching")
	}
	return a.ws, nil
}

func (a *Attachment) sendFrame(typeCode byte, payload []byte) error {
	ws, err := a.current()
	if err != nil {
		return err
	}
	if typeCode == frameStdin {
		err = ws.sendData(typeCode, payload)
	} else {
		err = ws.send(typeCode, payload)
	}
	if err != nil {
		return &Error{Kind: ErrUnavailable, Message: "the connection is down (reconnecting): " + err.Error(), SandboxID: a.sandbox.ID, cause: err}
	}
	return nil
}

// Write sends p to the session as terminal input.
func (a *Attachment) Write(p []byte) (int, error) {
	if err := a.sendFrame(frameStdin, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize changes the terminal size; it is kept for reconnects.
func (a *Attachment) Resize(cols, rows int) error {
	if err := checkDimensions(cols, rows, true); err != nil {
		return err
	}
	a.mu.Lock()
	a.cols, a.rows = cols, rows
	a.mu.Unlock()
	payload, _ := json.Marshal(resizeWire{Cols: uint32(cols), Rows: uint32(rows)})
	return a.sendFrame(frameResize, payload)
}

// Signal sends a signal by name (for example "INT", "TERM") to the
// session's process group.
func (a *Attachment) Signal(signal string) error {
	if signal == "" {
		return invalidArgument("signal is required")
	}
	payload, _ := json.Marshal(struct {
		Signal string `json:"signal"`
	}{signal})
	return a.sendFrame(frameSignal, payload)
}

// Detach leaves the session running and ends the attachment after the
// session confirms (or after a short wait). Safe to call more than once.
func (a *Attachment) Detach() error {
	ws, err := a.current()
	if err != nil {
		return nil
	}
	a.mu.Lock()
	a.detaching = true
	a.mu.Unlock()
	if ws.send(frameDetach, nil) == nil {
		timer := time.NewTimer(attachDetachWait)
		defer timer.Stop()
		select {
		case <-a.done:
			return nil
		case <-timer.C:
		}
	}
	ws.closeNormal()
	<-a.done
	return nil
}

// Close drops the connection at once; the session keeps running (closing
// detaches, it never kills). Safe to call more than once.
func (a *Attachment) Close() error {
	a.mu.Lock()
	a.detaching = true
	ws := a.ws
	a.mu.Unlock()
	a.cancel() // also stops a reconnect in progress
	if ws != nil {
		ws.closeNormal()
	}
	<-a.done
	return nil
}

// Wait blocks until the attachment ends and reports how. An error is an
// *Error: the connection could not be kept or restored.
func (a *Attachment) Wait() (*AttachResult, error) {
	<-a.done
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result, a.err
}

// Done is closed when the attachment has ended.
func (a *Attachment) Done() <-chan struct{} { return a.done }
