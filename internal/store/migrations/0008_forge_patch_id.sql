-- The patch id of a bot pull request's diff as its forge reports it, so a
-- rebase that changed nothing is skipped before a runner is made for it.
-- It is compared only with other forge patch ids: the forge's diff is not
-- the runner's byte for byte.
ALTER TABLE reviews ADD COLUMN forge_patch_id text NOT NULL DEFAULT '';
