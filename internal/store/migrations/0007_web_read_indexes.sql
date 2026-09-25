-- Indexes for the dashboard's read queries (internal/store/web_reads*.go).
-- Lists are keyset-paginated newest first, with the row id as the tiebreak;
-- row-level security adds the tenant_id predicate, so each list's index
-- leads with it.

-- A review's cost and a review's usage rows.
CREATE INDEX usage_review_id_idx ON usage (review_id) WHERE review_id IS NOT NULL;

-- A review's newest runner run.
CREATE INDEX runner_runs_review_created_idx ON runner_runs (review_id, created_at DESC) WHERE review_id IS NOT NULL;

-- The pull request, index run and follow-up lists.
CREATE INDEX pull_requests_tenant_updated_idx ON pull_requests (tenant_id, updated_at DESC, id DESC);
CREATE INDEX index_runs_tenant_created_idx ON index_runs (tenant_id, created_at DESC, id DESC);
CREATE INDEX followups_tenant_created_idx ON followups (tenant_id, created_at DESC, id DESC);

-- A follow-up looked up by the comment it answered.
CREATE INDEX followups_tenant_comment_idx ON followups (tenant_id, comment_id);

-- The repository list, and a repository looked up by its full name.
CREATE INDEX repositories_tenant_name_idx ON repositories (tenant_id, name, id);
