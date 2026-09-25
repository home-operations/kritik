-- The typed review contract: findings carry an explanation and an optional
-- suggested fix, severities are blocking / important / nit, and a
-- fingerprint (path and normalised title) recognises the same finding
-- across reviews. Existing rows keep their text and map error -> blocking,
-- warning -> important, info -> nit.
ALTER TABLE findings RENAME COLUMN body TO explanation;
ALTER TABLE findings ADD COLUMN suggested_fix text NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN fingerprint   text NOT NULL DEFAULT '';
ALTER TABLE findings DROP CONSTRAINT findings_severity_check;
UPDATE findings SET severity = CASE severity
    WHEN 'error'   THEN 'blocking'
    WHEN 'warning' THEN 'important'
    WHEN 'info'    THEN 'nit'
    ELSE severity
END;
ALTER TABLE findings ADD CONSTRAINT findings_severity_check CHECK (severity IN ('blocking', 'important', 'nit'));
-- The same hash review.Fingerprint computes: path, a NUL byte, then the
-- title lowercased with whitespace runs collapsed.
UPDATE findings SET fingerprint = encode(sha256(
    convert_to(path, 'UTF8') || '\x00'::bytea ||
    convert_to(btrim(regexp_replace(lower(title), '\s+', ' ', 'g')), 'UTF8')), 'hex');

-- summary holds the contract's summary (take and praise). mode, scope and
-- prior_review_id describe how the review was run: a single completion or
-- an agentic loop, over the whole diff or only what changed since the
-- prior review.
ALTER TABLE reviews ADD COLUMN summary         jsonb;
ALTER TABLE reviews ADD COLUMN mode            text NOT NULL DEFAULT 'single' CHECK (mode IN ('single', 'agentic'));
ALTER TABLE reviews ADD COLUMN prior_review_id uuid REFERENCES reviews (id);
ALTER TABLE reviews ADD COLUMN scope           text NOT NULL DEFAULT 'full' CHECK (scope IN ('full', 'incremental'));
ALTER TABLE reviews ADD COLUMN scope_reason    text NOT NULL DEFAULT '';
