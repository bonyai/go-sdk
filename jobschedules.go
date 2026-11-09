package bonya

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"time"
)

// Overlap says what a schedule does when its previous run is still going.
type Overlap string

const (
	// OverlapSkip drops the new fire (the default).
	OverlapSkip      Overlap = "skip"
	OverlapBufferOne Overlap = "buffer_one"
	OverlapAllowAll  Overlap = "allow_all"
)

// JobScheduleSpec describes a schedule: when to fire and what job to run.
//
// Exactly one of CronExpressions, IntervalSeconds and RunAt is required. A
// RunAt schedule is a one-shot: it fires once at that time.
type JobScheduleSpec struct {
	CronExpressions []string
	IntervalSeconds int
	// RunAt is a single future time; the service refuses a time in the past.
	RunAt time.Time
	// TimeZone is an IANA zone such as "US/Pacific"; empty is UTC.
	TimeZone      string
	JitterSeconds int
	// Overlap defaults to OverlapSkip.
	Overlap Overlap
	// Paused creates the schedule paused.
	Paused bool
	// Job is what each fire runs.
	Job JobSpec
	// IdempotencyKey makes creating the schedule safe to repeat. Empty
	// generates one per call. Ignored by UpdateJobSchedule.
	IdempotencyKey string
}

type scheduleWire struct {
	CronExpressions []string `json:"cron_expressions,omitempty"`
	IntervalSeconds int      `json:"interval_s,omitempty"`
	RunAtNanos      int64    `json:"run_at_unix_nanos,omitempty"`
	TimeZone        string   `json:"time_zone,omitempty"`
	JitterSeconds   int      `json:"jitter_s,omitempty"`
	Overlap         Overlap  `json:"overlap,omitempty"`
	Paused          bool     `json:"paused"`
	Job             JobSpec  `json:"job"`
}

func (s JobScheduleSpec) wire() (scheduleWire, error) {
	if s.IntervalSeconds < 0 || s.JitterSeconds < 0 {
		return scheduleWire{}, invalidArgument("IntervalSeconds and JitterSeconds must not be negative")
	}
	switch s.Overlap {
	case "", OverlapSkip, OverlapBufferOne, OverlapAllowAll:
	default:
		return scheduleWire{}, invalidArgument("Overlap must be \"skip\", \"buffer_one\" or \"allow_all\"")
	}
	if !s.RunAt.IsZero() && (s.RunAt.Year() < 1970 || s.RunAt.Year() > 2200) {
		return scheduleWire{}, invalidArgument("RunAt is out of range")
	}
	n := 0
	if len(s.CronExpressions) > 0 {
		n++
	}
	if s.IntervalSeconds > 0 {
		n++
	}
	if !s.RunAt.IsZero() {
		n++
	}
	if n != 1 {
		return scheduleWire{}, invalidArgument("set exactly one of CronExpressions, IntervalSeconds and RunAt")
	}
	if err := checkJobSpec(s.Job); err != nil {
		return scheduleWire{}, err
	}
	w := scheduleWire{
		CronExpressions: s.CronExpressions,
		IntervalSeconds: s.IntervalSeconds,
		TimeZone:        s.TimeZone,
		JitterSeconds:   s.JitterSeconds,
		Overlap:         s.Overlap,
		Paused:          s.Paused,
		Job:             s.Job,
	}
	if !s.RunAt.IsZero() {
		w.RunAtNanos = s.RunAt.UnixNano()
	}
	return w, nil
}

// JobScheduleTiming is the timing a schedule reports.
type JobScheduleTiming struct {
	CronExpressions []string `json:"cron_expressions,omitempty"`
	IntervalSeconds int      `json:"interval_s,omitempty"`
	RunAtNanos      int64    `json:"run_at_unix_nanos,omitempty"`
	TimeZone        string   `json:"time_zone,omitempty"`
	JitterSeconds   int      `json:"jitter_s,omitempty"`
	Overlap         Overlap  `json:"overlap,omitempty"`
}

// JobSchedule is a durable cron, interval or one-shot trigger for a job.
type JobSchedule struct {
	ID     string            `json:"schedule_id"`
	Timing JobScheduleTiming `json:"schedule"`
	Job    *JobSpec          `json:"job,omitempty"`
	Paused bool              `json:"paused"`
	Note   string            `json:"note,omitempty"`
	// NextRunAtNanos is when the schedule fires next; zero when none.
	NextRunAtNanos int64 `json:"next_run_at_unix_nanos,omitempty"`
	OneShot        bool  `json:"one_shot"`
	// RemainingActions counts the fires a one-shot has left (0 once spent).
	// It is 0 for every other schedule too: check OneShot before reading 0
	// as "spent".
	RemainingActions int `json:"remaining_actions,omitempty"`
	// AvailableActions is what the caller may do now: pause, resume, trigger,
	// update, delete.
	AvailableActions []string `json:"available_actions"`
	CreatedAtNanos   int64    `json:"created_at_unix_nanos,omitempty"`
	UpdatedAtNanos   int64    `json:"updated_at_unix_nanos,omitempty"`
	// NumActions counts fires, including manual triggers.
	NumActions               int `json:"num_actions,omitempty"`
	NumActionsSkippedOverlap int `json:"num_actions_skipped_overlap,omitempty"`
	NumActionsMissedCatchup  int `json:"num_actions_missed_catchup_window,omitempty"`
	// RecentRunIDs are the last ten fires, oldest first; each is a valid
	// GetJobRun id.
	RecentRunIDs  []string `json:"recent_run_ids,omitempty"`
	RunningRunIDs []string `json:"running_run_ids,omitempty"`
}

// NextRunAt returns when the schedule fires next, or the zero time.
func (s JobSchedule) NextRunAt() time.Time { return nanosToTime(s.NextRunAtNanos) }

// JobSchedulePage is one page of schedules.
type JobSchedulePage struct {
	Schedules     []JobSchedule `json:"schedules"`
	NextPageToken string        `json:"next_page_token"`
}

func schedulePath(id string, parts ...string) (string, error) {
	if id == "" {
		return "", invalidArgument("schedule id is required")
	}
	p := "/v1/job-schedule/" + url.PathEscape(id)
	for _, part := range parts {
		p += "/" + part
	}
	return p, nil
}

type scheduleEnvelope struct {
	Schedule JobSchedule `json:"schedule"`
}

// CreateJobSchedule creates a schedule.
func (c *Client) CreateJobSchedule(ctx context.Context, spec JobScheduleSpec) (*JobSchedule, error) {
	w, err := spec.wire()
	if err != nil {
		return nil, err
	}
	var out scheduleEnvelope
	if err := c.doJSON(ctx, &request{
		method: http.MethodPost, path: "/v1/job-schedule", json: w, idempotencyKey: keyOrNew(spec.IdempotencyKey),
	}, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

// GetJobSchedule returns one schedule.
func (c *Client) GetJobSchedule(ctx context.Context, scheduleID string) (*JobSchedule, error) {
	path, err := schedulePath(scheduleID)
	if err != nil {
		return nil, err
	}
	var out scheduleEnvelope
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path}, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

// UpdateJobSchedule replaces a schedule's timing and job.
func (c *Client) UpdateJobSchedule(ctx context.Context, scheduleID string, spec JobScheduleSpec) (*JobSchedule, error) {
	path, err := schedulePath(scheduleID)
	if err != nil {
		return nil, err
	}
	w, err := spec.wire()
	if err != nil {
		return nil, err
	}
	var out scheduleEnvelope
	if err := c.doJSON(ctx, &request{method: http.MethodPatch, path: path, json: w, idempotent: true}, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

// DeleteJobSchedule deletes a schedule. Runs it already started continue.
func (c *Client) DeleteJobSchedule(ctx context.Context, scheduleID string) error {
	path, err := schedulePath(scheduleID)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: path, idempotent: true}, nil)
}

// SetJobSchedulePaused pauses or resumes a schedule. Note is an optional
// reason stored with it.
func (c *Client) SetJobSchedulePaused(ctx context.Context, scheduleID string, paused bool, note string) (*JobSchedule, error) {
	path, err := schedulePath(scheduleID, "pause")
	if err != nil {
		return nil, err
	}
	body := struct {
		Paused bool   `json:"paused"`
		Note   string `json:"note,omitempty"`
	}{paused, note}
	var out scheduleEnvelope
	if err := c.doJSON(ctx, &request{method: http.MethodPost, path: path, json: body, idempotent: true}, &out); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

// TriggerJobSchedule fires one run immediately, overriding the schedule's own
// timing. It is not retried after a lost connection (ErrOutcomeUnknown): check
// the schedule's RecentRunIDs before trying again.
func (c *Client) TriggerJobSchedule(ctx context.Context, scheduleID string) error {
	path, err := schedulePath(scheduleID, "trigger")
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodPost, path: path}, nil)
}

// ListJobSchedules returns one page of schedules.
func (c *Client) ListJobSchedules(ctx context.Context, pageSize int, pageToken string) (*JobSchedulePage, error) {
	q, err := pageQuery(pageSize, pageToken)
	if err != nil {
		return nil, err
	}
	var out JobSchedulePage
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/job-schedule", query: q}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllJobSchedules iterates over every schedule. It stops after the first
// error, which it yields once.
func (c *Client) ListAllJobSchedules(ctx context.Context, pageSize int) iter.Seq2[JobSchedule, error] {
	return paginate(func(token string) ([]JobSchedule, string, error) {
		page, err := c.ListJobSchedules(ctx, pageSize, token)
		if err != nil {
			return nil, "", err
		}
		return page.Schedules, page.NextPageToken, nil
	})
}
