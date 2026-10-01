-- job_heartbeats is a replica's own sign of life on a River job it is
-- working: stamped as the job starts and every so often until Work
-- returns. River itself treats a running job as stuck only after the
-- longest job timeout has passed, so a job whose replica died outright
-- would otherwise hold its pull request, and its unique key, for hours;
-- the leader rescues one whose heartbeat has gone stale instead. Not
-- account-scoped, as River's own rows are not.
CREATE TABLE job_heartbeats (
    job_id       bigint      PRIMARY KEY,
    heartbeat_at timestamptz NOT NULL DEFAULT now()
);

-- river_job_id is the River job that started the run, so a run whose job
-- is no longer running, left by a replica that died, can be found and its
-- Kubernetes Job deleted rather than left to run beside the retry.
ALTER TABLE runner_runs ADD COLUMN IF NOT EXISTS river_job_id bigint;
CREATE INDEX IF NOT EXISTS runner_runs_unfinished_job_idx ON runner_runs (river_job_id) WHERE finished_at IS NULL;
