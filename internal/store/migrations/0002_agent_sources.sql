-- The URLs an agentic review's run tool gave curl (ADR-0008), recorded by
-- the runner and listed by the sticky comment as the sources consulted.
ALTER TABLE agent_runs ADD COLUMN sources jsonb NOT NULL DEFAULT '[]'::jsonb;
