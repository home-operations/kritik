-- The leader's onboarding feeder asks River's table, every half minute,
-- which repositories have an index job queued, running or lately failed,
-- by the repository id in each job's args. River's own index on args is
-- GIN, which a path lookup cannot use, and River keeps finished jobs for
-- days, so without this one each pass scans every index job per
-- repository. River's migrations run first, so the table exists here.
CREATE INDEX river_job_index_repository_idx ON river_job ((args->>'repository_id')) WHERE kind = 'index';
