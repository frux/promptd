package store

import (
	"errors"
	"time"
)

var (
	ErrNotFound          = errors.New("store: not found")
	ErrInvalidTransition = errors.New("store: invalid run transition")
)

type RunStatus string

const (
	RunQueued      RunStatus = "queued"
	RunRunning     RunStatus = "running"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunTimedOut    RunStatus = "timed_out"
	RunCanceled    RunStatus = "canceled"
	RunSkipped     RunStatus = "skipped"
	RunInterrupted RunStatus = "interrupted"
)

type JobSpec struct {
	ID           string
	ConfigHash   string
	ScheduleHash string
	ConfigJSON   []byte
}

type Job struct {
	ID           string
	ConfigHash   string
	ScheduleHash string
	ConfigJSON   []byte
	Enabled      bool
	UpdatedAt    time.Time
}

type SchedulerState struct {
	JobID           string
	NextRun         *time.Time
	LastScheduledAt *time.Time
	UpdatedAt       time.Time
}

type NewRun struct {
	JobID       string
	Trigger     string
	ScheduledAt *time.Time
}

type Run struct {
	ID          int64
	JobID       string
	ConfigHash  string
	Status      RunStatus
	Trigger     string
	ScheduledAt *time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	ExitCode    *int
	Error       string
	LogPath     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type RunResult struct {
	Status   RunStatus
	ExitCode *int
	Error    string
	LogPath  string
}

type RunFilter struct {
	JobID string
	Limit int
}

type JobStatus struct {
	Job       Job
	Scheduler SchedulerState
	LastRun   *Run
}

func (r RunStatus) terminal() bool {
	switch r {
	case RunSucceeded, RunFailed, RunTimedOut, RunCanceled, RunSkipped, RunInterrupted:
		return true
	default:
		return false
	}
}
