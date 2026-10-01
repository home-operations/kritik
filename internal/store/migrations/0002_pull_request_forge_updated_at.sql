-- forge_updated_at is when the forge last changed the pull request, as the
-- event or the poll that recorded the row said; NULL until one says. An
-- event older than it is stale and does not touch the row, so a delivery
-- that arrives late or again cannot rewind the head.
ALTER TABLE pull_requests ADD COLUMN IF NOT EXISTS forge_updated_at timestamptz;
