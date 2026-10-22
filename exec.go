package bonya

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// execMaxTimeout is the edge's upper bound for a command's timeout; it
	// bounds a buffered exec whose context has no deadline.
	execMaxTimeout = 10 * time.Minute
	// execDeadlineMargin covers the edge holding the call while the
	// sandbox wakes (up to 30 s) plus the network.
	execDeadlineMargin = 35 * time.Second
	// execMaxOutputBytes caps stdout and stderr each in an ExecResult, like
	// the edge does for a buffered exec.
	execMaxOutputBytes = 4 << 20
	// execCancelWait is how long a stopped stream waits for the command's
	// exit after asking it to stop.
	execCancelWait = 5 * time.Second
)

// ExecOptions describes a command to run in a sandbox.
type ExecOptions struct {
	// Command is the program and its arguments. Required.
	Command []string
	// Env adds environment variables.
	Env map[string]string
	// WorkingDir is the directory the command starts in.
	WorkingDir string
	// Timeout bounds the command. For Exec without a stream, zero means the
	// service default (60 s); the service caps it at 10 minutes.
	Timeout time.Duration
	// IdempotencyKey identifies this run. The SDK picks a random one when it
	// is empty. Running again with the same key never starts a second
	// process: it attaches to the first one and returns its output and exit
	// status (while the sandbox still remembers the key, 10 minutes after
	// exit). Set it yourself to recover after ErrOutcomeUnknown.
	IdempotencyKey string

	// Stdin is copied to the command's stdin, which is then closed.
	// Setting it runs the command over a stream.
	Stdin io.Reader
	// Stdout and Stderr receive output as it arrives. Setting either runs
	// the command over a stream. With TTY, all output goes to Stdout.
	Stdout io.Writer
	Stderr io.Writer
	// TTY runs the command in a terminal of Cols x Rows (each at most 512).
	TTY  bool
	Cols int
	Rows int
}

func (o *ExecOptions) validate() error {
	if len(o.Command) == 0 || o.Command[0] == "" {
		return invalidArgument("command is required")
	}
	if o.Timeout < 0 {
		return invalidArgument("timeout must not be negative")
	}
	if !o.TTY && (o.Cols != 0 || o.Rows != 0) {
		return invalidArgument("cols and rows need TTY")
	}
	return checkDimensions(o.Cols, o.Rows, false)
}

func (o *ExecOptions) streamed() bool {
	return o.Stdin != nil || o.Stdout != nil || o.Stderr != nil || o.TTY
}

// ExecResult is the outcome of a command that ran to its end.
type ExecResult struct {
	ExitStatus
	// Stdout and Stderr hold the output, each capped at 4 MiB.
	Stdout string
	Stderr string
	// StdoutTruncated and StderrTruncated report output beyond the cap.
	StdoutTruncated bool
	StderrTruncated bool
	// Duration is how long the command ran, as the service measured it
	// (for a streamed run, as the SDK measured it).
	Duration time.Duration
	// IdempotencyKey is the key the run used.
	IdempotencyKey string
}

// Check returns an *ExitError when the command did not exit with code 0.
func (r *ExecResult) Check() error {
	if r.ExitCode == 0 && !r.Signaled {
		return nil
	}
	return &ExitError{ExitStatus: r.ExitStatus, Stderr: r.Stderr}
}

// ExitError reports a command that ran but did not succeed. It is returned
// by ExecResult.Check, never by Exec itself.
type ExitError struct {
	ExitStatus
	// Stderr is the command's stderr, as captured.
	Stderr string
}

func (e *ExitError) Error() string {
	if e.Signaled {
		return "bonya: command killed by signal " + strconv.Itoa(e.Signal)
	}
	return "bonya: command exited with code " + strconv.Itoa(e.ExitCode)
}

type execRequestWire struct {
	Command    []string          `json:"command"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	TimeoutMS  int64             `json:"timeout_ms,omitempty"`
}

type execResponseWire struct {
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	StdoutTruncated bool    `json:"stdout_truncated"`
	StderrTruncated bool    `json:"stderr_truncated"`
	ExitCode        int32   `json:"exit_code"`
	Signaled        bool    `json:"signaled"`
	Signal          int32   `json:"signal"`
	TookMS          float64 `json:"took_ms"`
}

// Exec runs command and waits for it to exit. A non-zero exit is not an
// error; use ExecResult.Check.
func (s *Sandbox) Exec(ctx context.Context, command ...string) (*ExecResult, error) {
	return s.ExecWithOptions(ctx, ExecOptions{Command: command})
}

// ExecWithOptions runs a command and waits for it to exit. Without Stdin,
// Stdout, Stderr or TTY it is one request; otherwise it runs over a stream
// and also copies output to the given writers.
//
// The run carries an idempotency key, so the SDK resends it after a
// transient failure: the sandbox attaches the resend to the same process.
// When the outcome cannot be known the error is ErrOutcomeUnknown and
// nothing is resent; run again with the same IdempotencyKey to get the
// result without starting the command twice.
func (s *Sandbox) ExecWithOptions(ctx context.Context, opts ExecOptions) (*ExecResult, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.IdempotencyKey == "" {
		opts.IdempotencyKey = newIdempotencyKey()
	}
	if opts.streamed() {
		return s.execCollect(ctx, opts)
	}

	execCtx, cancel := ctx, context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		bound := opts.Timeout
		if bound <= 0 || bound > execMaxTimeout {
			bound = execMaxTimeout
		}
		execCtx, cancel = context.WithTimeout(ctx, bound+execDeadlineMargin)
	}
	defer cancel()

	var out execResponseWire
	err := s.edgeJSON(execCtx, &request{
		method: http.MethodPost,
		path:   sandboxPath(s.ID, "exec"),
		json: execRequestWire{
			Command:    opts.Command,
			Env:        opts.Env,
			WorkingDir: opts.WorkingDir,
			TimeoutMS:  opts.Timeout.Milliseconds(),
		},
		idempotencyKey: opts.IdempotencyKey,
		// The command's own timeout bounds the call, not the client default.
		stream: true,
	}, &out)
	if err != nil {
		return nil, execError(ctx, execCtx, err, s.ID)
	}
	return &ExecResult{
		ExitStatus:      ExitStatus{ExitCode: int(out.ExitCode), Signaled: out.Signaled, Signal: int(out.Signal)},
		Stdout:          out.Stdout,
		Stderr:          out.Stderr,
		StdoutTruncated: out.StdoutTruncated,
		StderrTruncated: out.StderrTruncated,
		Duration:        time.Duration(out.TookMS * float64(time.Millisecond)),
		IdempotencyKey:  opts.IdempotencyKey,
	}, nil
}

// execError reports an exec whose effect cannot be known as
// ErrOutcomeUnknown: a keyed command keeps running in the sandbox when its
// call breaks or times out.
func execError(parent, execCtx context.Context, err error, sandboxID string) error {
	if parent.Err() == nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return &Error{Kind: ErrOutcomeUnknown, SandboxID: sandboxID, cause: context.DeadlineExceeded,
			Message: "the command did not finish within its timeout and may still be running"}
	}
	var e *Error
	if !errors.As(err, &e) || !errors.Is(e, ErrUnavailable) {
		return err
	}
	if e.Code == "upstream_unavailable" && e.Message == "exec timed out" {
		return &Error{Kind: ErrOutcomeUnknown, Code: e.Code, SandboxID: sandboxID, cause: e,
			Message: "the command did not finish within its timeout and may still be running"}
	}
	return err
}

// execCollect runs opts over a stream and buffers its output.
func (s *Sandbox) execCollect(ctx context.Context, opts ExecOptions) (*ExecResult, error) {
	stdout := &cappedBuffer{limit: execMaxOutputBytes}
	stderr := &cappedBuffer{limit: execMaxOutputBytes}
	streamOpts := opts
	streamOpts.Stdout = teeWriter(stdout, opts.Stdout)
	streamOpts.Stderr = teeWriter(stderr, opts.Stderr)
	begun := time.Now()
	stream, err := s.ExecStream(ctx, streamOpts)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	exit, err := stream.Wait()
	if err != nil {
		return nil, err
	}
	return &ExecResult{
		ExitStatus:      *exit,
		Stdout:          stdout.buf.String(),
		Stderr:          stderr.buf.String(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		Duration:        time.Since(begun),
		IdempotencyKey:  opts.IdempotencyKey,
	}, nil
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if len(p) > room {
		b.truncated = true
		b.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.buf.Write(p)
}

func teeWriter(buffer io.Writer, user io.Writer) io.Writer {
	if user == nil {
		return buffer
	}
	return io.MultiWriter(buffer, user)
}

// ExecStream is a running command with live input and output. Output goes
// to the writers in ExecOptions; Write sends stdin. Call Wait for the exit
// status and Close when done.
type ExecStream struct {
	sandboxID string
	key       string
	ws        *wsConn
	stdout    io.Writer
	stderr    io.Writer

	mu          sync.Mutex
	stdinClosed bool
	stopping    bool
	stopCause   error
	localErr    error // a failing output writer or Stdin reader

	done chan struct{}
	exit *ExitStatus
	err  error

	stopWatch func() bool
	cancel    context.CancelFunc
}

type execStartWire struct {
	Command    []string          `json:"command"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	TTY        bool              `json:"tty,omitempty"`
	Cols       uint32            `json:"cols,omitempty"`
	Rows       uint32            `json:"rows,omitempty"`
}

// ExecStream starts a command over a stream and returns once it is sent.
// Cancelling ctx, Timeout passing, or Close stops the command. If the
// connection breaks instead, the command keeps running; Wait reports
// ErrOutcomeUnknown, and ExecStream with the same IdempotencyKey attaches
// to it again (its kept output, up to 1 MiB, is sent again first).
//
// The edge has no signal frame for exec; Cancel ends the command.
func (s *Sandbox) ExecStream(ctx context.Context, opts ExecOptions) (*ExecStream, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.IdempotencyKey == "" {
		opts.IdempotencyKey = newIdempotencyKey()
	}
	ws, err := s.dialEdge(ctx, sandboxPath(s.ID, "exec"), opts.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	start := execStartWire{
		Command:    opts.Command,
		Env:        opts.Env,
		WorkingDir: opts.WorkingDir,
		TTY:        opts.TTY,
		Cols:       uint32(opts.Cols),
		Rows:       uint32(opts.Rows),
	}
	if err := ws.sendJSON(frameStart, start); err != nil {
		ws.closeNormal()
		// Nothing runs before the edge has read START.
		return nil, &Error{Kind: ErrUnavailable, Message: "send START: " + err.Error(), SandboxID: s.ID, cause: err}
	}

	e := &ExecStream{
		sandboxID: s.ID,
		key:       opts.IdempotencyKey,
		ws:        ws,
		stdout:    orDiscard(opts.Stdout),
		stderr:    orDiscard(opts.Stderr),
		done:      make(chan struct{}),
	}
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	e.cancel = cancel
	e.stopWatch = context.AfterFunc(runCtx, func() { e.stop(context.Cause(runCtx)) })
	go e.read()
	if opts.Stdin != nil {
		go e.copyStdin(opts.Stdin)
	}
	return e, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// IdempotencyKey returns the run's key; ExecStream with it attaches again.
func (e *ExecStream) IdempotencyKey() string { return e.key }

func (e *ExecStream) read() {
	var errFrame *wsErrorFrame
	var stdoutFailed, stderrFailed bool
	for {
		typeCode, payload, err := e.ws.recv()
		if err != nil {
			e.finish(nil, err, errFrame)
			return
		}
		switch typeCode {
		case frameOutput:
			if !stdoutFailed {
				if _, werr := e.stdout.Write(payload); werr != nil {
					stdoutFailed = true
					e.noteLocal(fmt.Errorf("write stdout: %w", werr))
				}
			}
		case frameStderr:
			if !stderrFailed {
				if _, werr := e.stderr.Write(payload); werr != nil {
					stderrFailed = true
					e.noteLocal(fmt.Errorf("write stderr: %w", werr))
				}
			}
		case frameExit:
			exit, perr := parseExit(payload, e.sandboxID)
			e.finish(exit, perr, nil)
			e.ws.closeNormal()
			return
		case frameError:
			errFrame = parseErrorFrame(payload)
		default:
			// Frame types this SDK version does not know are skipped.
		}
	}
}

func (e *ExecStream) noteLocal(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.localErr == nil {
		e.localErr = err
	}
}

// finish records the end of the stream once.
func (e *ExecStream) finish(exit *ExitStatus, readErr error, errFrame *wsErrorFrame) {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.done:
		return
	default:
	}
	switch {
	case exit != nil:
		e.exit = exit
		if e.localErr != nil {
			e.err = &Error{Kind: ErrInvalidRequest, Message: e.localErr.Error(), SandboxID: e.sandboxID, cause: e.localErr}
		}
	case e.stopping:
		e.err = &Error{Kind: ErrOutcomeUnknown, SandboxID: e.sandboxID, cause: e.stopCause,
			Message: "the stream was stopped before the command exited; the command was asked to stop"}
	default:
		var sdkErr *Error
		if errFrame == nil && errors.As(readErr, &sdkErr) {
			e.err = sdkErr
		} else {
			e.err = streamFailure(e.sandboxID, errFrame, readErr, streamExec)
		}
	}
	close(e.done)
	if e.stopWatch != nil {
		e.stopWatch()
	}
	e.cancel()
}

func (e *ExecStream) finished() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// stop asks the command to end, waits briefly for its exit, then drops the
// connection. A keyed command outlives its connection, so the cancel is
// sent explicitly first.
func (e *ExecStream) stop(cause error) {
	e.mu.Lock()
	if e.stopping || e.finished() {
		e.mu.Unlock()
		return
	}
	e.stopping, e.stopCause = true, cause
	e.mu.Unlock()
	if e.ws.send(frameCancel, nil) == nil {
		timer := time.NewTimer(execCancelWait)
		select {
		case <-e.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	e.ws.closeNormal()
	<-e.done
}

func (e *ExecStream) copyStdin(r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := e.Write(buf[:n]); werr != nil {
				return
			}
		}
		if errors.Is(err, io.EOF) {
			_ = e.CloseStdin()
			return
		}
		if err != nil {
			e.noteLocal(fmt.Errorf("read stdin: %w", err))
			_ = e.CloseStdin()
			return
		}
	}
}

// Write sends p to the command's stdin.
func (e *ExecStream) Write(p []byte) (int, error) {
	e.mu.Lock()
	closed := e.stdinClosed
	e.mu.Unlock()
	if closed {
		return 0, invalidArgument("stdin is closed")
	}
	if err := e.sendFrame(frameStdin, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseStdin closes the command's stdin (end of input).
func (e *ExecStream) CloseStdin() error {
	e.mu.Lock()
	if e.stdinClosed {
		e.mu.Unlock()
		return nil
	}
	e.stdinClosed = true
	e.mu.Unlock()
	return e.sendFrame(frameStdinEOF, nil)
}

// Resize changes the terminal size of a TTY command.
func (e *ExecStream) Resize(cols, rows int) error {
	if err := checkDimensions(cols, rows, true); err != nil {
		return err
	}
	payload, _ := json.Marshal(resizeWire{Cols: uint32(cols), Rows: uint32(rows)})
	return e.sendFrame(frameResize, payload)
}

// Cancel asks the command to end (its process group is terminated). Wait
// then returns its exit status.
func (e *ExecStream) Cancel() error {
	return e.sendFrame(frameCancel, nil)
}

func (e *ExecStream) sendFrame(typeCode byte, payload []byte) error {
	if e.finished() {
		if e.err != nil {
			return e.err
		}
		return invalidArgument("the command has exited")
	}
	var err error
	if typeCode == frameStdin {
		err = e.ws.sendData(typeCode, payload)
	} else {
		err = e.ws.send(typeCode, payload)
	}
	if err != nil {
		return &Error{Kind: ErrUnavailable, Message: "the stream is closed: " + err.Error(), SandboxID: e.sandboxID, cause: err}
	}
	return nil
}

// Wait blocks until the command exits and returns its status. An error is
// an *Error; ErrOutcomeUnknown means the stream ended before the exit.
func (e *ExecStream) Wait() (*ExitStatus, error) {
	<-e.done
	return e.exit, e.err
}

// Done is closed when the stream has ended.
func (e *ExecStream) Done() <-chan struct{} { return e.done }

// Close stops the command if it is still running and releases the stream.
// It is safe to call more than once and after Wait.
func (e *ExecStream) Close() error {
	if !e.finished() {
		e.stop(nil)
	}
	return nil
}
