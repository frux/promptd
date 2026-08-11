package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)

func TestOpenCreatesPrivateDatabaseAndMigratesIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "promptd.db")

	store, err := Open(ctx, path, WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(state dir) error = %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("state directory mode = %o, want 700", got)
	}

	store, err = Open(ctx, path, WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer store.Close()

	var migrations int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrations != 2 {
		t.Fatalf("migration count = %d, want 2", migrations)
	}
}

func TestOpenDoesNotChangeParentDirectoryPermissions(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}

	openTestStoreAt(t, filepath.Join(parent, "promptd.db"))
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("parent directory mode = %o, want 755", got)
	}
}

func TestOpenReadOnlyReadsWithoutAllowingWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "promptd.db")
	writable := openTestStoreAt(t, path)
	seedJob(t, writable, "report")
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer readOnly.Close()
	jobs, err := readOnly.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "report" {
		t.Fatalf("jobs = %#v", jobs)
	}
	if err := readOnly.ReconcileJobs(ctx, nil); err == nil {
		t.Fatal("ReconcileJobs() on read-only store error = nil")
	}
}

func TestOpenReadOnlyRejectsMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenReadOnly(context.Background(), path); err == nil {
		t.Fatal("OpenReadOnly() error = nil")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database was created: %v", err)
	}
}

func TestReconcileJobsDisablesMissingJobs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.ReconcileJobs(ctx, []JobSpec{
		{ID: "alpha", ConfigHash: "hash-a", ScheduleHash: "schedule-a", ConfigJSON: []byte(`{"id":"alpha"}`)},
		{ID: "beta", ConfigHash: "hash-b", ScheduleHash: "schedule-b", ConfigJSON: []byte(`{"id":"beta"}`)},
	}); err != nil {
		t.Fatalf("ReconcileJobs() error = %v", err)
	}
	if err := store.ReconcileJobs(ctx, []JobSpec{
		{ID: "beta", ConfigHash: "hash-b2", ScheduleHash: "schedule-b", ConfigJSON: []byte(`{"id":"beta","v":2}`)},
	}); err != nil {
		t.Fatalf("second ReconcileJobs() error = %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs() error = %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	if jobs[0].ID != "alpha" || jobs[0].Enabled {
		t.Fatalf("alpha = %#v, want disabled", jobs[0])
	}
	if jobs[1].ID != "beta" || !jobs[1].Enabled || jobs[1].ConfigHash != "hash-b2" {
		t.Fatalf("beta = %#v, want enabled with updated hash", jobs[1])
	}
}

func TestSchedulerState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedJob(t, store, "report")

	next := fixedNow.Add(time.Hour)
	last := fixedNow.Add(-time.Hour)
	if err := store.SetSchedulerState(ctx, "report", &next, &last); err != nil {
		t.Fatalf("SetSchedulerState() error = %v", err)
	}

	state, err := store.SchedulerState(ctx, "report")
	if err != nil {
		t.Fatalf("SchedulerState() error = %v", err)
	}
	if state.NextRun == nil || !state.NextRun.Equal(next) {
		t.Fatalf("next run = %v, want %v", state.NextRun, next)
	}
	if state.LastScheduledAt == nil || !state.LastScheduledAt.Equal(last) {
		t.Fatalf("last scheduled = %v, want %v", state.LastScheduledAt, last)
	}

	if err := store.SetSchedulerState(ctx, "missing", &next, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing state error = %v, want ErrNotFound", err)
	}
}

func TestReconcileJobsResetsOnlyChangedSchedules(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	spec := JobSpec{
		ID: "report", ConfigHash: "config-v1", ScheduleHash: "schedule-v1", ConfigJSON: []byte(`{"version":1}`),
	}
	if err := store.ReconcileJobs(ctx, []JobSpec{spec}); err != nil {
		t.Fatal(err)
	}
	next := fixedNow.Add(time.Hour)
	last := fixedNow
	if err := store.SetSchedulerState(ctx, "report", &next, &last); err != nil {
		t.Fatal(err)
	}

	spec.ConfigHash = "config-v2"
	spec.ConfigJSON = []byte(`{"version":2}`)
	if err := store.ReconcileJobs(ctx, []JobSpec{spec}); err != nil {
		t.Fatal(err)
	}
	state, err := store.SchedulerState(ctx, "report")
	if err != nil {
		t.Fatal(err)
	}
	if state.NextRun == nil || !state.NextRun.Equal(next) {
		t.Fatalf("next run after config-only change = %v, want %v", state.NextRun, next)
	}

	spec.ScheduleHash = "schedule-v2"
	if err := store.ReconcileJobs(ctx, []JobSpec{spec}); err != nil {
		t.Fatal(err)
	}
	state, err = store.SchedulerState(ctx, "report")
	if err != nil {
		t.Fatal(err)
	}
	if state.NextRun != nil || state.LastScheduledAt != nil {
		t.Fatalf("state after schedule change = %#v, want reset", state)
	}
}

func TestRunLifecycleAndHistory(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedJob(t, store, "report")

	scheduled := fixedNow.Add(time.Minute)
	run, err := store.CreateRun(ctx, NewRun{JobID: "report", Trigger: "scheduled", ScheduledAt: &scheduled})
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if run.Status != RunQueued {
		t.Fatalf("status = %q, want queued", run.Status)
	}
	if run.ConfigHash != "hash-report" {
		t.Fatalf("config hash = %q, want hash-report", run.ConfigHash)
	}

	started := fixedNow.Add(2 * time.Minute)
	run, err = store.StartRun(ctx, run.ID, started, "/tmp/report.log")
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	if run.Status != RunRunning || run.StartedAt == nil || !run.StartedAt.Equal(started) {
		t.Fatalf("started run = %#v", run)
	}

	exitCode := 0
	finished := fixedNow.Add(3 * time.Minute)
	run, err = store.FinishRun(ctx, run.ID, finished, RunResult{
		Status:   RunSucceeded,
		ExitCode: &exitCode,
		LogPath:  "/tmp/report.log",
	})
	if err != nil {
		t.Fatalf("FinishRun() error = %v", err)
	}
	if run.Status != RunSucceeded || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("finished run = %#v", run)
	}

	runs, err := store.ListRuns(ctx, RunFilter{JobID: "report", Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %#v", runs)
	}

	if _, err := store.StartRun(ctx, run.ID, started, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second StartRun() error = %v, want ErrInvalidTransition", err)
	}
	if _, err := store.StartRun(ctx, 9999, started, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing StartRun() error = %v, want ErrNotFound", err)
	}
}

func TestFinishQueuedRunAsSkippedOrCanceled(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedJob(t, store, "report")

	for _, status := range []RunStatus{RunSkipped, RunCanceled} {
		run, err := store.CreateRun(ctx, NewRun{JobID: "report", Trigger: "scheduled"})
		if err != nil {
			t.Fatal(err)
		}
		run, err = store.FinishRun(ctx, run.ID, fixedNow, RunResult{Status: status})
		if err != nil {
			t.Fatalf("FinishRun(%q) error = %v", status, err)
		}
		if run.Status != status {
			t.Fatalf("status = %q, want %q", run.Status, status)
		}
	}
}

func TestRecoverInterruptedRunsMarksAllUnfinishedRuns(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedJob(t, store, "report")

	running, err := store.CreateRun(ctx, NewRun{JobID: "report", Trigger: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRun(ctx, running.ID, fixedNow, "/tmp/running.log"); err != nil {
		t.Fatal(err)
	}
	queued, err := store.CreateRun(ctx, NewRun{JobID: "report", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}

	count, err := store.RecoverInterruptedRuns(ctx, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("RecoverInterruptedRuns() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("recovered count = %d, want 2", count)
	}

	running, err = store.Run(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != RunInterrupted || running.FinishedAt == nil {
		t.Fatalf("recovered run = %#v", running)
	}
	queued, err = store.Run(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != RunInterrupted || queued.FinishedAt == nil {
		t.Fatalf("recovered queued run = %#v", queued)
	}
}

func TestCreateRunRejectsUnknownJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.CreateRun(ctx, NewRun{JobID: "missing", Trigger: "manual"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateRun() error = %v, want ErrNotFound", err)
	}
}

func TestCreateRunRejectsDisabledJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedJob(t, store, "retired")
	if err := store.ReconcileJobs(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(ctx, NewRun{JobID: "retired", Trigger: "manual"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateRun() error = %v, want ErrNotFound", err)
	}
}

func TestRunKeepsOriginalJobRevision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.ReconcileJobs(ctx, []JobSpec{{
		ID: "report", ConfigHash: "hash-v1", ScheduleHash: "schedule-v1", ConfigJSON: []byte(`{"version":1}`),
	}}); err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, NewRun{JobID: "report", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileJobs(ctx, []JobSpec{{
		ID: "report", ConfigHash: "hash-v2", ScheduleHash: "schedule-v1", ConfigJSON: []byte(`{"version":2}`),
	}}); err != nil {
		t.Fatal(err)
	}

	run, err = store.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ConfigHash != "hash-v1" {
		t.Fatalf("run config hash = %q, want hash-v1", run.ConfigHash)
	}

	var revisions int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM job_revisions WHERE job_id = 'report'`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 2 {
		t.Fatalf("revision count = %d, want 2", revisions)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreAt(t, filepath.Join(t.TempDir(), "promptd.db"))
}

func openTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(context.Background(), path, WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedJob(t *testing.T, store *Store, id string) {
	t.Helper()
	if err := store.ReconcileJobs(context.Background(), []JobSpec{{
		ID: id, ConfigHash: "hash-" + id, ScheduleHash: "schedule-" + id, ConfigJSON: []byte(`{"id":"` + id + `"}`),
	}}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}
