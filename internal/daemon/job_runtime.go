package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/frux/promptd/internal/config"
	"github.com/frux/promptd/internal/control"
	"github.com/frux/promptd/internal/runner"
	"github.com/frux/promptd/internal/scheduler"
	"github.com/frux/promptd/internal/store"
	"github.com/frux/promptd/internal/supervisor"
)

type jobRunner interface {
	Run(context.Context, config.Job, io.Writer) (supervisor.Result, error)
}

type jobRuntime struct {
	ctx      context.Context
	cancel   context.CancelFunc
	store    *store.Store
	runner   jobRunner
	logDir   string
	logger   *slog.Logger
	mu       sync.Mutex
	jobs     map[string]config.Job
	activity map[string]*jobActivity
	closed   bool
	wg       sync.WaitGroup
	stopOnce sync.Once
}

type jobActivity struct {
	running bool
	queued  *runEvent
}

type runEvent struct {
	scheduler.Event
	manual bool
}

const stateOperationTimeout = 5 * time.Second

func newJobRuntime(
	parent context.Context,
	state *store.Store,
	jobRunner jobRunner,
	logDir string,
	logger *slog.Logger,
	jobs map[string]config.Job,
) (*jobRuntime, error) {
	if logDir == "" {
		return nil, fmt.Errorf("log directory is empty")
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &jobRuntime{
		ctx:      ctx,
		cancel:   cancel,
		store:    state,
		runner:   jobRunner,
		logDir:   logDir,
		logger:   logger,
		activity: make(map[string]*jobActivity),
	}
	runtime.Replace(jobs)
	return runtime, nil
}

func (r *jobRuntime) Replace(jobs map[string]config.Job) {
	copyOfJobs := make(map[string]config.Job, len(jobs))
	for id, job := range jobs {
		copyOfJobs[id] = job
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = copyOfJobs
	for id, activity := range r.activity {
		if _, exists := copyOfJobs[id]; !exists {
			activity.queued = nil
		}
	}
}

func (r *jobRuntime) Trigger(event scheduler.Event) {
	if _, err := r.trigger(runEvent{Event: event}); err != nil {
		r.logger.Warn("scheduled job rejected", "job", event.JobID, "error", err)
	}
}

func (r *jobRuntime) RunJob(jobID string) (control.RunResponse, error) {
	status, err := r.trigger(runEvent{
		Event:  scheduler.Event{JobID: jobID, ScheduledAt: time.Now().UTC()},
		manual: true,
	})
	if err != nil {
		return control.RunResponse{}, err
	}
	return control.RunResponse{Version: control.APIVersion, JobID: jobID, Status: status}, nil
}

func (r *jobRuntime) trigger(event runEvent) (string, error) {
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		return "", control.ErrStopping
	}
	job, exists := r.jobs[event.JobID]
	if !exists {
		r.mu.Unlock()
		return "", fmt.Errorf("job %q: %w", event.JobID, control.ErrUnknownJob)
	}
	activity := r.activity[event.JobID]
	if activity == nil {
		activity = &jobActivity{}
		r.activity[event.JobID] = activity
	}
	if activity.running {
		if job.Run.Overlap == "queue_one" && activity.queued == nil {
			queued := event
			activity.queued = &queued
			r.mu.Unlock()
			r.logger.Info("job occurrence queued", "job", event.JobID, "scheduled_at", event.ScheduledAt)
			return control.RunQueued, nil
		}
		r.wg.Add(1)
		r.mu.Unlock()
		go func() {
			defer r.wg.Done()
			r.recordSkipped(event, "previous run is still active")
		}()
		return control.RunSkipped, nil
	}
	activity.running = true
	r.wg.Add(1)
	r.mu.Unlock()
	go r.execute(job, event)
	return control.RunAccepted, nil
}

func (r *jobRuntime) Shutdown() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		for _, activity := range r.activity {
			activity.queued = nil
		}
		r.cancel()
		r.mu.Unlock()
		r.wg.Wait()
	})
}

func (r *jobRuntime) execute(job config.Job, event runEvent) {
	defer r.complete(event.JobID)

	record, err := r.createRun(store.NewRun{
		JobID:       event.JobID,
		Trigger:     eventTrigger(event),
		ScheduledAt: event.scheduledAt(),
	})
	if err != nil {
		r.logger.Error("create run record failed", "job", event.JobID, "error", err)
		return
	}

	logFile, logPath, err := r.openLog(event.JobID, record.ID, event.ScheduledAt)
	if err != nil {
		r.failBeforeExecution(record.ID, event.JobID, fmt.Errorf("open run log: %w", err))
		return
	}
	startedAt := time.Now().UTC()
	if _, err := r.startRun(record.ID, startedAt, logPath); err != nil {
		_ = logFile.Close()
		r.logger.Error("start run record failed", "job", event.JobID, "run", record.ID, "error", err)
		return
	}
	_, _ = fmt.Fprintf(logFile, "promptd: job=%s run=%d started_at=%s\n", event.JobID, record.ID, startedAt.Format(time.RFC3339Nano))

	result, runErr := r.runner.Run(r.ctx, job, logFile)
	status, message := classifyResult(result, runErr)
	finishedAt := result.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}
	_, _ = fmt.Fprintf(logFile, "promptd: status=%s exit_code=%d finished_at=%s\n", status, result.ExitCode, finishedAt.Format(time.RFC3339Nano))
	if err := logFile.Close(); err != nil {
		r.logger.Error("close run log failed", "job", event.JobID, "run", record.ID, "error", err)
	}
	exitCode := result.ExitCode
	if _, err := r.finishRun(record.ID, finishedAt, store.RunResult{
		Status:   status,
		ExitCode: &exitCode,
		Error:    message,
		LogPath:  logPath,
	}); err != nil {
		r.logger.Error("finish run record failed", "job", event.JobID, "run", record.ID, "error", err)
		return
	}
	r.logger.Info("job run finished", "job", event.JobID, "run", record.ID, "status", status, "exit_code", exitCode)
}

func (r *jobRuntime) failBeforeExecution(runID int64, jobID string, failure error) {
	startedAt := time.Now().UTC()
	if _, err := r.startRun(runID, startedAt, ""); err != nil {
		r.logger.Error("start failed run record failed", "job", jobID, "run", runID, "error", err)
		return
	}
	exitCode := -1
	if _, err := r.finishRun(runID, time.Now().UTC(), store.RunResult{
		Status:   store.RunFailed,
		ExitCode: &exitCode,
		Error:    failure.Error(),
	}); err != nil {
		r.logger.Error("finish failed run record failed", "job", jobID, "run", runID, "error", err)
	}
}

func (r *jobRuntime) recordSkipped(event runEvent, reason string) {
	record, err := r.createRun(store.NewRun{
		JobID:       event.JobID,
		Trigger:     eventTrigger(event),
		ScheduledAt: event.scheduledAt(),
	})
	if err != nil {
		r.logger.Error("create skipped run failed", "job", event.JobID, "error", err)
		return
	}
	if _, err := r.finishRun(record.ID, time.Now().UTC(), store.RunResult{
		Status: store.RunSkipped,
		Error:  reason,
	}); err != nil {
		r.logger.Error("finish skipped run failed", "job", event.JobID, "run", record.ID, "error", err)
		return
	}
	r.logger.Info("job occurrence skipped", "job", event.JobID, "reason", reason)
}

func (r *jobRuntime) complete(jobID string) {
	var nextJob config.Job
	var nextEvent runEvent
	var startNext bool

	r.mu.Lock()
	activity := r.activity[jobID]
	if activity != nil {
		activity.running = false
		if !r.closed && activity.queued != nil {
			if job, exists := r.jobs[jobID]; exists {
				nextJob = job
				nextEvent = *activity.queued
				activity.queued = nil
				activity.running = true
				r.wg.Add(1)
				startNext = true
			}
		}
	}
	r.mu.Unlock()

	if startNext {
		go r.execute(nextJob, nextEvent)
	}
	r.wg.Done()
}

func (r *jobRuntime) openLog(jobID string, runID int64, scheduledAt time.Time) (*os.File, string, error) {
	directory := filepath.Join(r.logDir, jobID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%s-%d.log", scheduledAt.UTC().Format("20060102T150405.000000000Z"), runID)
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	return file, path, nil
}

func classifyResult(result supervisor.Result, err error) (store.RunStatus, string) {
	switch {
	case result.TimedOut:
		if err != nil {
			return store.RunTimedOut, err.Error()
		}
		return store.RunTimedOut, "run timed out"
	case result.Canceled || errors.Is(err, context.Canceled):
		if err != nil {
			return store.RunCanceled, err.Error()
		}
		return store.RunCanceled, "run canceled"
	case err != nil:
		return store.RunFailed, err.Error()
	case result.ExitCode == 0:
		return store.RunSucceeded, ""
	default:
		return store.RunFailed, fmt.Sprintf("process exited with code %d", result.ExitCode)
	}
}

func (event runEvent) scheduledAt() *time.Time {
	if event.manual {
		return nil
	}
	return &event.ScheduledAt
}

func eventTrigger(event runEvent) string {
	if event.manual {
		return "manual"
	}
	if event.Misfired {
		return "misfire"
	}
	return "scheduled"
}

func (r *jobRuntime) createRun(input store.NewRun) (store.Run, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateOperationTimeout)
	defer cancel()
	return r.store.CreateRun(ctx, input)
}

func (r *jobRuntime) startRun(id int64, startedAt time.Time, logPath string) (store.Run, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateOperationTimeout)
	defer cancel()
	return r.store.StartRun(ctx, id, startedAt, logPath)
}

func (r *jobRuntime) finishRun(id int64, finishedAt time.Time, result store.RunResult) (store.Run, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateOperationTimeout)
	defer cancel()
	return r.store.FinishRun(ctx, id, finishedAt, result)
}

var _ jobRunner = (*runner.Service)(nil)
