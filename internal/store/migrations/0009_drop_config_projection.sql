-- The runtime reads the merged configuration snapshot, never these partial
-- projections of the file, which ApplyConfig wrote and nothing read
-- (ADR-0010 §2.1).
ALTER TABLE tenants DROP COLUMN settings;
ALTER TABLE repositories DROP COLUMN settings;
