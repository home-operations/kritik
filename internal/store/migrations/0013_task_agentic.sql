-- Agentic tasks (ADR-0012) run in a runner like an agentic review: the run
-- is a runner_runs row of kind task, which its task run points at, and the
-- runner's gateway token is granted for the task run in place of a review.
-- notes say what a run's context left out, and why: the worker's on the
-- task run, the runner's on its agent run, from which the worker copies
-- them.
ALTER TABLE runner_runs DROP CONSTRAINT runner_runs_kind_check;
ALTER TABLE runner_runs ADD CONSTRAINT runner_runs_kind_check CHECK (kind IN ('review', 'index', 'task'));

ALTER TABLE task_runs ADD COLUMN runner_run_id uuid REFERENCES runner_runs (id);
-- runner_started_at is when a job handed the run to its runner: a job that
-- finds it set and no answer was cut off while the agent ran.
ALTER TABLE task_runs ADD COLUMN runner_started_at timestamptz;
ALTER TABLE task_runs ADD COLUMN notes text[] NOT NULL DEFAULT '{}';

ALTER TABLE gateway_tokens ALTER COLUMN review_id DROP NOT NULL;
ALTER TABLE gateway_tokens ADD COLUMN task_run_id uuid REFERENCES task_runs (id);
ALTER TABLE gateway_tokens ADD CONSTRAINT gateway_tokens_subject_check CHECK ((review_id IS NULL) <> (task_run_id IS NULL));

ALTER TABLE agent_runs ADD COLUMN notes text[] NOT NULL DEFAULT '{}';
