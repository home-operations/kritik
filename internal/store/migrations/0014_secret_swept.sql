-- The leader deletes each run's job-scoped Secret by name once the run can
-- no longer need it, and stamps secret_swept_at so the row is not visited
-- again. The partial index keeps the sweep's scan to rows still pending.
ALTER TABLE runner_runs ADD COLUMN secret_swept_at timestamptz;
CREATE INDEX runner_runs_secret_pending_idx ON runner_runs (tenant_id, created_at) WHERE secret_swept_at IS NULL;
