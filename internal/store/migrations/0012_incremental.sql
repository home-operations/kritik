-- prior_head_sha is the head of the last completed review when the runner
-- could fetch it, NULL when there was none or it could not be fetched (a
-- force-push made it unreachable, say); delta_diff is the diff from it to
-- the head, and delta_paths the paths it touches that no ignore glob covers.
ALTER TABLE context_packs ADD COLUMN prior_head_sha text;
ALTER TABLE context_packs ADD COLUMN delta_diff     text   NOT NULL DEFAULT '';
ALTER TABLE context_packs ADD COLUMN delta_paths    text[] NOT NULL DEFAULT '{}';

-- posted_inline is true when an inline comment for the finding is on the
-- forge: posted by its own review, or by an earlier one whose finding had
-- the same fingerprint, which is why this review did not post it again.
-- Findings from before this migration count as not posted, so the first
-- re-review after the upgrade posts their inline comments once more.
ALTER TABLE findings ADD COLUMN posted_inline boolean NOT NULL DEFAULT false;
