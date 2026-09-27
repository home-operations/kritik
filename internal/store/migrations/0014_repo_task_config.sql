-- The .kritik.yaml a task dispatch last resolved at the repository's
-- default branch tip (ADR-0012), so the dashboard lists the tasks that run
-- rather than those of the file a review read at a merge base.
-- task_config_sha is '' until a dispatch has resolved one;
-- task_config_doc is NULL when the tip has no usable file.
ALTER TABLE repositories ADD COLUMN task_config_sha text NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN task_config_doc text;
ALTER TABLE repositories ADD COLUMN task_config_at timestamptz;
