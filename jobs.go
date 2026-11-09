package bonya

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// JobScript is a script the caller supplies. It travels inline and is capped
// by the service; a larger program belongs in the template.
type JobScript struct {
	// Body is the script text.
	Body []byte `json:"body_b64"`
	// Interpreter runs the script; empty means "sh".
	Interpreter string   `json:"interpreter,omitempty"`
	Args        []string `json:"args,omitempty"`
	Filename    string   `json:"filename,omitempty"`
}

// JobNewSandbox asks a job to create its own sandbox.
type JobNewSandbox struct {
	// TemplateID is empty to use the deployment's default template.
	TemplateID string `json:"template_id,omitempty"`
	Version    string `json:"version,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Disposition says what happens to a sandbox a job created once it ends.
type Disposition string

const (
	DispositionDelete Disposition = "delete"
	DispositionKeep   Disposition = "keep"
)

// JobSpec describes a job: where it runs and what it runs.
//
// Exactly one of ExistingSandboxID and NewSandbox is required, and exactly one
// of Cmd and Script.
type JobSpec struct {
	// Name is an optional human-readable name for the job.
	Name              string         `json:"name,omitempty"`
	ExistingSandboxID string         `json:"existing_sandbox_id,omitempty"`
	NewSandbox        *JobNewSandbox `json:"new_sandbox,omitempty"`
	Cmd               []string       `json:"cmd,omitempty"`
	Script            *JobScript     `json:"script,omitempty"`
	// PreRunScript runs after the sandbox is ready and before the job; a
	// non-zero exit stops the job.
	PreRunScript *JobScript        `json:"pre_run_script,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	// Path is the working directory.
	Path  string `json:"path,omitempty"`
	Stdin []byte `json:"stdin_b64,omitempty"`
	// CommandTimeoutSeconds bounds the command; RunDeadlineSeconds bounds the
	// whole run. Zero uses the service defaults.
	CommandTimeoutSeconds int `json:"command_timeout_s,omitempty"`
	RunDeadlineSeconds    int `json:"run_deadline_s,omitempty"`
	MaxOutputBytes        int `json:"max_output_bytes,omitempty"`
	// Disposition applies to a sandbox the run created; empty means delete.
	Disposition Disposition `json:"disposition,omitempty"`
	// ResumeIfSuspended lets the run resume a suspended existing sandbox,
	// which is billable. When false a suspended target fails the run.
	ResumeIfSuspended bool `json:"resume_if_suspended,omitempty"`

	// IdempotencyKey makes starting the job safe to repeat: the same key joins
	// the first call's run. Empty generates one per call, kept across the
	// SDK's own retries.
	IdempotencyKey string `json:"-"`
}

// JobStart is the answer to StartJob.
type JobStart struct {
	RunID string `json:"run_id"`
	// AlreadyRunning is true when the call joined an existing run, as a retry
	// with the same IdempotencyKey does.
	AlreadyRunning bool `json:"already_running"`
}

// JobResult is a finished command's outcome. A non-zero ExitCode is a
// completed run, not a failed one.
type JobResult struct {
	ExitCode        int    `json:"exit_code"`
	Signaled        bool   `json:"signaled"`
	Signal          int    `json:"signal,omitempty"`
	TimedOut        bool   `json:"timed_out"`
	Stdout          []byte `json:"stdout_b64"`
	Stderr          []byte `json:"stderr_b64"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

// JobStatus is the state of a run.
type JobStatus string

const (
	JobRunning    JobStatus = "running"
	JobCompleted  JobStatus = "completed"
	JobFailed     JobStatus = "failed"
	JobCanceled   JobStatus = "canceled"
	JobTimedOut   JobStatus = "timed_out"
	JobTerminated JobStatus = "terminated"
)

// Done reports whether the run has ended. Only the known final statuses count:
// an unrecognized status (one a newer server adds) is treated as still in
// progress, so a polling loop keeps going rather than stopping early.
func (s JobStatus) Done() bool {
	switch s {
	case JobCompleted, JobFailed, JobCanceled, JobTimedOut, JobTerminated:
		return true
	}
	return false
}

// JobRun is one run of a job.
type JobRun struct {
	ID     string    `json:"run_id"`
	Name   string    `json:"name,omitempty"`
	Status JobStatus `json:"status"`
	// SandboxID is the sandbox the run used.
	SandboxID string `json:"sandbox_id,omitempty"`
	// CreatedSandbox is true when the run created SandboxID itself.
	CreatedSandbox  bool   `json:"created_sandbox"`
	ScheduleID      string `json:"schedule_id,omitempty"`
	StartedAtNanos  int64  `json:"started_at_unix_nanos,omitempty"`
	FinishedAtNanos int64  `json:"finished_at_unix_nanos,omitempty"`
	// Result is set once the command has run.
	Result  *JobResult `json:"result,omitempty"`
	Failure string     `json:"failure,omitempty"`
	// CleanupFailed means the run could not delete the sandbox it created;
	// that sandbox is still billed.
	CleanupFailed bool `json:"cleanup_failed"`
	// AvailableActions is what the caller may do now: cancel, rerun, retry,
	// delete, delete_sandbox.
	AvailableActions []string `json:"available_actions"`
}

// StartedAt returns when the run started, or the zero time.
func (r JobRun) StartedAt() time.Time { return nanosToTime(r.StartedAtNanos) }

// FinishedAt returns when the run finished, or the zero time.
func (r JobRun) FinishedAt() time.Time { return nanosToTime(r.FinishedAtNanos) }

// JobTimelineEntry is one activity attempt in a run's history.
type JobTimelineEntry struct {
	Name            string `json:"name"`
	Attempt         int    `json:"attempt,omitempty"`
	StartedAtNanos  int64  `json:"started_at_unix_nanos,omitempty"`
	FinishedAtNanos int64  `json:"finished_at_unix_nanos,omitempty"`
	Failure         string `json:"failure,omitempty"`
}

// JobRunDetail is a run with the spec that started it and its timeline.
type JobRunDetail struct {
	JobRun
	Job      *JobSpec           `json:"job,omitempty"`
	Timeline []JobTimelineEntry `json:"timeline,omitempty"`
}

// ListJobRunsOptions configures ListJobRuns and ListAllJobRuns.
type ListJobRunsOptions struct {
	// SandboxID and ScheduleID filter the listing when set.
	SandboxID  string
	ScheduleID string
	PageSize   int
	PageToken  string
}

// JobRunPage is one page of runs, newest first.
type JobRunPage struct {
	Runs          []JobRun `json:"runs"`
	NextPageToken string   `json:"next_page_token"`
}

func nanosToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func jobPath(runID string, parts ...string) (string, error) {
	if runID == "" {
		return "", invalidArgument("run id is required")
	}
	p := "/v1/job/" + url.PathEscape(runID)
	for _, part := range parts {
		p += "/" + part
	}
	return p, nil
}

func pageQuery(size int, token string) (url.Values, error) {
	if size < 0 {
		return nil, invalidArgument("page size must not be negative")
	}
	q := url.Values{}
	if size > 0 {
		q.Set("page_size", strconv.Itoa(size))
	}
	if token != "" {
		q.Set("page_token", token)
	}
	return q, nil
}

func checkJobSpec(spec JobSpec) error {
	switch spec.Disposition {
	case "", DispositionDelete, DispositionKeep:
	default:
		return invalidArgument("Disposition must be \"delete\" or \"keep\"")
	}
	if (spec.ExistingSandboxID == "") == (spec.NewSandbox == nil) {
		return invalidArgument("set exactly one of ExistingSandboxID and NewSandbox")
	}
	if (len(spec.Cmd) == 0) == (spec.Script == nil) {
		return invalidArgument("set exactly one of Cmd and Script")
	}
	return nil
}

// StartJob starts a job durably and returns its run id without waiting for the
// result; poll with GetJobRun.
func (c *Client) StartJob(ctx context.Context, spec JobSpec) (*JobStart, error) {
	if err := checkJobSpec(spec); err != nil {
		return nil, err
	}
	var out JobStart
	if err := c.doJSON(ctx, &request{
		method:         http.MethodPost,
		path:           "/v1/job",
		json:           spec,
		idempotencyKey: keyOrNew(spec.IdempotencyKey),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunJob starts a job and waits for it to end, polling GetJobRun.
//
// The wait is bounded by ctx, or by a five-minute default when ctx has no
// deadline. If the wait ends first, RunJob returns the error together with a
// non-nil *JobRun that carries the run id (the run keeps going on the
// service), so the caller can keep polling with GetJobRun or stop it with
// CancelJobRun. When the job could not be started at all the *JobRun is nil.
func (c *Client) RunJob(ctx context.Context, spec JobSpec) (*JobRun, error) {
	ctx, cancel := withLongTimeout(ctx)
	defer cancel()
	start, err := c.StartJob(ctx, spec)
	if err != nil {
		return nil, err
	}
	run := &JobRun{ID: start.RunID, Status: JobRunning}
	delay := pollStart
	for {
		detail, err := c.GetJobRun(ctx, start.RunID)
		if err != nil {
			return run, err
		}
		run = &detail.JobRun
		if run.Status.Done() {
			return run, nil
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return run, &Error{Kind: ErrTimeout, Message: "stopped waiting for job run " + start.RunID + "; it is still running", cause: err}
		}
		if delay *= 2; delay > pollMax {
			delay = pollMax
		}
	}
}

// GetJobRun returns a run's full detail, including the spec that started it
// and its activity timeline.
func (c *Client) GetJobRun(ctx context.Context, runID string) (*JobRunDetail, error) {
	path, err := jobPath(runID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Run JobRunDetail `json:"run"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path}, &out); err != nil {
		return nil, err
	}
	return &out.Run, nil
}

// CancelJobRun requests cancellation. The run's own cleanup (such as deleting
// a sandbox it created) still executes.
func (c *Client) CancelJobRun(ctx context.Context, runID string) error {
	path, err := jobPath(runID, "cancel")
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodPost, path: path, idempotent: true}, nil)
}

// ListJobRuns returns one page of the organization's runs, newest first.
func (c *Client) ListJobRuns(ctx context.Context, opts ListJobRunsOptions) (*JobRunPage, error) {
	q, err := pageQuery(opts.PageSize, opts.PageToken)
	if err != nil {
		return nil, err
	}
	if opts.SandboxID != "" {
		q.Set("sandbox_id", opts.SandboxID)
	}
	if opts.ScheduleID != "" {
		q.Set("schedule_id", opts.ScheduleID)
	}
	var out JobRunPage
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/job", query: q}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllJobRuns iterates over every page of a listing. It stops after the
// first error, which it yields once.
func (c *Client) ListAllJobRuns(ctx context.Context, opts ListJobRunsOptions) iter.Seq2[JobRun, error] {
	return paginate(func(token string) ([]JobRun, string, error) {
		o := opts
		if token != "" {
			o.PageToken = token
		}
		page, err := c.ListJobRuns(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return page.Runs, page.NextPageToken, nil
	})
}

// keyOrNew returns key, or a fresh idempotency key when it is empty. Callers
// keep the result across their own retries.
func keyOrNew(key string) string {
	if key != "" {
		return key
	}
	return newIdempotencyKey()
}

// paginate yields every item of a paged listing. page fetches the page after
// token (empty for the first) and returns its items and the next token. It
// stops after the first error, which it yields once.
func paginate[T any](page func(token string) ([]T, string, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		token := ""
		for {
			items, next, err := page(token)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, it := range items {
				if !yield(it, nil) {
					return
				}
			}
			if next == "" {
				return
			}
			token = next
		}
	}
}
