package control

import (
	"time"

	"github.com/frux/promptd/internal/store"
)

const APIVersion = 1

type StatusResponse struct {
	Version int         `json:"version"`
	Jobs    []JobStatus `json:"jobs"`
}

type JobStatus struct {
	ID      string     `json:"id"`
	Enabled bool       `json:"enabled"`
	NextRun *time.Time `json:"next_run,omitempty"`
	LastRun *RunStatus `json:"last_run,omitempty"`
}

type RunStatus struct {
	ID          int64           `json:"id"`
	Status      store.RunStatus `json:"status"`
	ScheduledAt *time.Time      `json:"scheduled_at,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	ExitCode    *int            `json:"exit_code,omitempty"`
	Error       string          `json:"error,omitempty"`
	LogPath     string          `json:"log_path,omitempty"`
}

func StatusFromStore(statuses []store.JobStatus) StatusResponse {
	response := StatusResponse{
		Version: APIVersion,
		Jobs:    make([]JobStatus, 0, len(statuses)),
	}
	for _, status := range statuses {
		job := JobStatus{ID: status.Job.ID, Enabled: status.Job.Enabled}
		if status.Job.Enabled {
			job.NextRun = status.Scheduler.NextRun
		}
		if status.LastRun != nil {
			run := status.LastRun
			job.LastRun = &RunStatus{
				ID:          run.ID,
				Status:      run.Status,
				ScheduledAt: run.ScheduledAt,
				StartedAt:   run.StartedAt,
				FinishedAt:  run.FinishedAt,
				ExitCode:    run.ExitCode,
				Error:       run.Error,
				LogPath:     run.LogPath,
			}
		}
		response.Jobs = append(response.Jobs, job)
	}
	return response
}
