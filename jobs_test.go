package bonya

import (
	"errors"
	"testing"
	"time"
)

func TestJobStatusDone(t *testing.T) {
	for s, want := range map[JobStatus]bool{
		JobRunning: false, "": false, "pending": false, "queued": false,
		JobCompleted: true, JobFailed: true, JobCanceled: true, JobTimedOut: true, JobTerminated: true,
	} {
		if got := s.Done(); got != want {
			t.Errorf("%q.Done() = %v, want %v", s, got, want)
		}
	}
}

func TestPaginateWalksPagesAndStopsOnError(t *testing.T) {
	pages := map[string][]int{"": {1, 2}, "a": {3}}
	next := map[string]string{"": "a", "a": ""}
	var got []int
	for v, err := range paginate(func(tok string) ([]int, string, error) { return pages[tok], next[tok], nil }) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[2] != 3 {
		t.Fatalf("got %v", got)
	}
	boom := errors.New("boom")
	n := 0
	for _, err := range paginate(func(string) ([]int, string, error) { return nil, "", boom }) {
		n++
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	}
	if n != 1 {
		t.Fatalf("yielded %d times, want 1", n)
	}
}

func TestScheduleSpecValidation(t *testing.T) {
	job := JobSpec{ExistingSandboxID: "sb-x", Cmd: []string{"true"}}
	bad := []JobScheduleSpec{
		{IntervalSeconds: -5, Job: job},
		{CronExpressions: []string{"* * * * *"}, JitterSeconds: -1, Job: job},
		{CronExpressions: []string{"* * * * *"}, Overlap: "sometimes", Job: job},
		{RunAt: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), Job: job},
		{CronExpressions: []string{"* * * * *"}, IntervalSeconds: 60, Job: job},
		{CronExpressions: []string{"* * * * *"}, Job: JobSpec{ExistingSandboxID: "sb-x", Cmd: []string{"true"}, Disposition: "Delete"}},
	}
	for i, s := range bad {
		if _, err := s.wire(); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
	if _, err := (JobScheduleSpec{IntervalSeconds: 60, Job: job}).wire(); err != nil {
		t.Errorf("valid spec rejected: %v", err)
	}
}
