-- The pull request description, used by the .kritik.yaml filter (pr.body)
-- and by review prompts.
ALTER TABLE pull_requests ADD COLUMN body text NOT NULL DEFAULT '';
