CREATE TABLE jobs (
    id          TEXT PRIMARY KEY,
    config_hash TEXT NOT NULL,
    config_json BLOB NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    updated_at  INTEGER NOT NULL
);

CREATE TABLE scheduler_state (
    job_id            TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    next_run           INTEGER,
    last_scheduled_at  INTEGER,
    updated_at         INTEGER NOT NULL
);

CREATE TABLE job_revisions (
    job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
    config_hash  TEXT NOT NULL,
    config_json  BLOB NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (job_id, config_hash)
);

CREATE TABLE runs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id        TEXT NOT NULL,
    config_hash   TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN (
        'queued',
        'running',
        'succeeded',
        'failed',
        'timed_out',
        'canceled',
        'skipped',
        'interrupted'
    )),
    trigger       TEXT NOT NULL,
    scheduled_at  INTEGER,
    started_at    INTEGER,
    finished_at   INTEGER,
    exit_code     INTEGER,
    error         TEXT NOT NULL DEFAULT '',
    log_path      TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    FOREIGN KEY (job_id, config_hash)
        REFERENCES job_revisions(job_id, config_hash) ON DELETE RESTRICT
);

CREATE INDEX runs_job_id_created_at_idx ON runs(job_id, created_at DESC);
CREATE INDEX runs_status_idx ON runs(status);
