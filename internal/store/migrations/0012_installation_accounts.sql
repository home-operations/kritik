-- An installation serves a list of accounts, as a public GitHub App
-- installed on several organizations does, so the column becomes a list and
-- dashboard tenants' stored specs name accounts the same way.
ALTER TABLE installations ADD COLUMN accounts text[] NOT NULL DEFAULT '{}';
UPDATE installations SET accounts = ARRAY[account] WHERE account <> '';
ALTER TABLE installations DROP COLUMN account;

UPDATE dashboard_tenants SET spec = jsonb_set(spec, '{installations}', (
    SELECT jsonb_agg(CASE WHEN i ? 'account'
            THEN (i - 'account') || jsonb_build_object('accounts', jsonb_build_array(i -> 'account'))
            ELSE i END ORDER BY n)
    FROM jsonb_array_elements(spec -> 'installations') WITH ORDINALITY AS e (i, n)))
WHERE jsonb_typeof(spec -> 'installations') = 'array' AND jsonb_array_length(spec -> 'installations') > 0;
