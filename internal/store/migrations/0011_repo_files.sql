-- repo_files holds .kritik.yaml and the files it and the operator name, as
-- read from the merge-base tree; repo_notes says what could not be read.
ALTER TABLE context_packs ADD COLUMN repo_files jsonb  NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE context_packs ADD COLUMN repo_notes text[] NOT NULL DEFAULT '{}';

-- skip_reason says why a review ended skipped by the repository's own
-- configuration.
ALTER TABLE reviews ADD COLUMN skip_reason text NOT NULL DEFAULT ''
    CHECK (skip_reason IN ('', 'disabled', 'filtered', 'only_skipped_paths'));
