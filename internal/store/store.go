package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Option func(*Store)

func WithClock(now func() time.Time) Option {
	return func(store *Store) {
		store.now = now
	}
}

func DefaultPath() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		return filepath.Join(stateHome, "promptd", "promptd.db"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "promptd", "promptd.db"), nil
	}
	return filepath.Join(home, ".local", "state", "promptd", "promptd.db"), nil
}

func Open(ctx context.Context, path string, options ...Option) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store path is empty")
	}

	if path != ":memory:" {
		stateDir := filepath.Dir(path)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create sqlite file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close sqlite file: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secure sqlite file: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{
		db:  db,
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		option(store)
	}

	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}

	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	}
	for _, pragma := range pragmas {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure sqlite with %q: %w", pragma, err)
		}
	}

	if err := s.migrate(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var current int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}

		body, err := migrationFiles.ReadFile(filepath.ToSlash(filepath.Join("migrations", entry.Name())))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if err := s.applyMigration(ctx, version, entry.Name(), string(body)); err != nil {
			return err
		}
		current = version
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q has no numeric prefix", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("migration %q has invalid version", name)
	}
	return version, nil
}

func (s *Store) applyMigration(ctx context.Context, version int, name, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
		version, name, toUnixNano(s.now()),
	); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

func (s *Store) ReconcileJobs(ctx context.Context, jobs []JobSpec) error {
	seen := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		if job.ID == "" || job.ConfigHash == "" || len(job.ConfigJSON) == 0 {
			return fmt.Errorf("job id, config hash, and config JSON are required")
		}
		if _, exists := seen[job.ID]; exists {
			return fmt.Errorf("duplicate job %q", job.ID)
		}
		seen[job.ID] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin job reconciliation: %w", err)
	}
	defer tx.Rollback()

	now := toUnixNano(s.now())
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET enabled = 0, updated_at = ? WHERE enabled = 1", now); err != nil {
		return fmt.Errorf("disable stale jobs: %w", err)
	}

	for _, job := range jobs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO jobs(id, config_hash, config_json, enabled, updated_at)
VALUES (?, ?, ?, 1, ?)
ON CONFLICT(id) DO UPDATE SET
    config_hash = excluded.config_hash,
    config_json = excluded.config_json,
    enabled = 1,
    updated_at = excluded.updated_at`, job.ID, job.ConfigHash, job.ConfigJSON, now); err != nil {
			return fmt.Errorf("upsert job %q: %w", job.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO scheduler_state(job_id, updated_at)
VALUES (?, ?)
ON CONFLICT(job_id) DO NOTHING`, job.ID, now); err != nil {
			return fmt.Errorf("initialize scheduler state for %q: %w", job.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO job_revisions(job_id, config_hash, config_json, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(job_id, config_hash) DO NOTHING`, job.ID, job.ConfigHash, job.ConfigJSON, now); err != nil {
			return fmt.Errorf("record job revision for %q: %w", job.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit job reconciliation: %w", err)
	}
	return nil
}

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, config_hash, config_json, enabled, updated_at
FROM jobs
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		var enabled int
		var updatedAt int64
		if err := rows.Scan(&job.ID, &job.ConfigHash, &job.ConfigJSON, &enabled, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		job.Enabled = enabled == 1
		job.UpdatedAt = fromUnixNano(updatedAt)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}
	return jobs, nil
}

func (s *Store) SetSchedulerState(ctx context.Context, jobID string, nextRun, lastScheduledAt *time.Time) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE scheduler_state
SET next_run = ?, last_scheduled_at = ?, updated_at = ?
WHERE job_id = ?`, nullableTime(nextRun), nullableTime(lastScheduledAt), toUnixNano(s.now()), jobID)
	if err != nil {
		return fmt.Errorf("update scheduler state for %q: %w", jobID, err)
	}
	return requireAffected(result, ErrNotFound)
}

func (s *Store) SchedulerState(ctx context.Context, jobID string) (SchedulerState, error) {
	var state SchedulerState
	var nextRun, lastScheduledAt sql.NullInt64
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT job_id, next_run, last_scheduled_at, updated_at
FROM scheduler_state
WHERE job_id = ?`, jobID).Scan(&state.JobID, &nextRun, &lastScheduledAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SchedulerState{}, ErrNotFound
	}
	if err != nil {
		return SchedulerState{}, fmt.Errorf("read scheduler state for %q: %w", jobID, err)
	}
	state.NextRun = fromNullableTime(nextRun)
	state.LastScheduledAt = fromNullableTime(lastScheduledAt)
	state.UpdatedAt = fromUnixNano(updatedAt)
	return state, nil
}

func (s *Store) CreateRun(ctx context.Context, input NewRun) (Run, error) {
	if input.JobID == "" || strings.TrimSpace(input.Trigger) == "" {
		return Run{}, fmt.Errorf("job id and trigger are required")
	}
	now := toUnixNano(s.now())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO runs(job_id, config_hash, status, trigger, scheduled_at, created_at, updated_at)
SELECT id, config_hash, ?, ?, ?, ?, ?
FROM jobs
WHERE id = ? AND enabled = 1`, RunQueued, input.Trigger, nullableTime(input.ScheduledAt), now, now, input.JobID)
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}
	if err := requireAffected(result, ErrNotFound); err != nil {
		return Run{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Run{}, fmt.Errorf("read run id: %w", err)
	}
	return s.Run(ctx, id)
}

func (s *Store) StartRun(ctx context.Context, id int64, startedAt time.Time) (Run, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE runs
SET status = ?, started_at = ?, updated_at = ?
WHERE id = ? AND status = ?`, RunRunning, toUnixNano(startedAt), toUnixNano(s.now()), id, RunQueued)
	if err != nil {
		return Run{}, fmt.Errorf("start run %d: %w", id, err)
	}
	if err := s.requireRunTransition(ctx, id, result); err != nil {
		return Run{}, err
	}
	return s.Run(ctx, id)
}

func (s *Store) FinishRun(ctx context.Context, id int64, finishedAt time.Time, output RunResult) (Run, error) {
	if !output.Status.terminal() || output.Status == RunInterrupted {
		return Run{}, fmt.Errorf("finish run %d: %w: %q is not an allowed result", id, ErrInvalidTransition, output.Status)
	}

	allowedFrom := []RunStatus{RunRunning}
	switch output.Status {
	case RunSkipped:
		allowedFrom = []RunStatus{RunQueued}
	case RunCanceled:
		allowedFrom = []RunStatus{RunQueued, RunRunning}
	}

	placeholders := make([]string, len(allowedFrom))
	args := []any{
		output.Status, toUnixNano(finishedAt), nullableInt(output.ExitCode), output.Error, output.LogPath,
		toUnixNano(s.now()), id,
	}
	for index, status := range allowedFrom {
		placeholders[index] = "?"
		args = append(args, status)
	}

	result, err := s.db.ExecContext(ctx, `
UPDATE runs
SET status = ?, finished_at = ?, exit_code = ?, error = ?, log_path = ?, updated_at = ?
WHERE id = ? AND status IN (`+strings.Join(placeholders, ", ")+")", args...)
	if err != nil {
		return Run{}, fmt.Errorf("finish run %d: %w", id, err)
	}
	if err := s.requireRunTransition(ctx, id, result); err != nil {
		return Run{}, err
	}
	return s.Run(ctx, id)
}

func (s *Store) RecoverInterruptedRuns(ctx context.Context, recoveredAt time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE runs
SET status = ?, finished_at = ?, error = CASE
        WHEN error = '' THEN 'daemon stopped before run completion'
        ELSE error
    END,
    updated_at = ?
WHERE status = ?`, RunInterrupted, toUnixNano(recoveredAt), toUnixNano(s.now()), RunRunning)
	if err != nil {
		return 0, fmt.Errorf("recover interrupted runs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count interrupted runs: %w", err)
	}
	return count, nil
}

func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, job_id, config_hash, status, trigger, scheduled_at, started_at, finished_at,
       exit_code, error, log_path, created_at, updated_at
FROM runs
WHERE id = ?`, id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("read run %d: %w", id, err)
	}
	return run, nil
}

func (s *Store) ListRuns(ctx context.Context, filter RunFilter) ([]Run, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	query := `
SELECT id, job_id, config_hash, status, trigger, scheduled_at, started_at, finished_at,
       exit_code, error, log_path, created_at, updated_at
FROM runs`
	var args []any
	if filter.JobID != "" {
		query += " WHERE job_id = ?"
		args = append(args, filter.JobID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return runs, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(row scanner) (Run, error) {
	var run Run
	var scheduledAt, startedAt, finishedAt sql.NullInt64
	var exitCode sql.NullInt64
	var createdAt, updatedAt int64
	err := row.Scan(
		&run.ID, &run.JobID, &run.ConfigHash, &run.Status, &run.Trigger, &scheduledAt, &startedAt, &finishedAt,
		&exitCode, &run.Error, &run.LogPath, &createdAt, &updatedAt,
	)
	if err != nil {
		return Run{}, err
	}
	run.ScheduledAt = fromNullableTime(scheduledAt)
	run.StartedAt = fromNullableTime(startedAt)
	run.FinishedAt = fromNullableTime(finishedAt)
	if exitCode.Valid {
		value := int(exitCode.Int64)
		run.ExitCode = &value
	}
	run.CreatedAt = fromUnixNano(createdAt)
	run.UpdatedAt = fromUnixNano(updatedAt)
	return run, nil
}

func (s *Store) requireRunTransition(ctx context.Context, id int64, result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count rows affected by run %d transition: %w", id, err)
	}
	if count > 0 {
		return nil
	}
	var exists int
	err = s.db.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE id = ?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check run %d after transition: %w", id, err)
	}
	return ErrInvalidTransition
}

func requireAffected(result sql.Result, emptyError error) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return emptyError
	}
	return nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return toUnixNano(*value)
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func fromNullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := fromUnixNano(value.Int64)
	return &result
}

func toUnixNano(value time.Time) int64 {
	return value.UTC().UnixNano()
}

func fromUnixNano(value int64) time.Time {
	return time.Unix(0, value).UTC()
}
