-- The runner stamps heartbeat_at while it works; the worker ends a run
-- whose heartbeat has gone stale instead of waiting out the Job deadline.
ALTER TABLE runner_runs ADD COLUMN heartbeat_at timestamptz;
