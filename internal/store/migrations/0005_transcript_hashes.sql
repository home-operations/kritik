-- The state an agent step's row leaves for the next step of its run
-- (internal/transcript): the request's message count and cumulative
-- message hash, the hashes of the system prompt and tool list, and the
-- bytes the run has recorded through this row. The next step's row is a
-- delta against it, and a request whose prefix no longer matches it is
-- recorded whole. Only agent steps read it back.
ALTER TABLE model_calls
    ADD COLUMN messages_end int    NOT NULL DEFAULT 0,
    ADD COLUMN messages_sha bytea,
    ADD COLUMN system_sha   bytea,
    ADD COLUMN tools_sha    bytea,
    ADD COLUMN run_bytes    bigint NOT NULL DEFAULT 0;

-- The retention sweep deletes by age across every tenant; a follow-up's
-- transcript is looked up by the comment it answered.
CREATE INDEX model_calls_created_at_idx ON model_calls (created_at);
CREATE INDEX model_calls_followup_comment_idx ON model_calls (followup_comment_id) WHERE followup_comment_id IS NOT NULL;
