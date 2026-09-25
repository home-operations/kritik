-- Dashboard sign-in hardening (ADR-0009 §§2.4, 2.7).

-- A login state is bound to the browser that started it: browser_hash is the
-- SHA-256 of a random value held in that browser's kritik_login cookie, so a
-- callback URL replayed into another browser (login CSRF) cannot complete.
-- In-flight states from before this column existed cannot be bound, and
-- expire within minutes anyway, so they are dropped.
DELETE FROM login_states;
ALTER TABLE login_states ADD COLUMN browser_hash bytea NOT NULL;
CREATE INDEX login_states_expires_at_idx ON login_states (expires_at);

-- An identity is keyed by the sign-in's origin as well as its name: the
-- forge type and base URL, or the OIDC issuer. Pointing a sign-in name at
-- another host or issuer then yields new identities instead of letting a
-- subject on the new origin take over an account from the old one. Rows
-- from before the column keep the empty origin, which no sign-in has, so
-- they are never matched again.
ALTER TABLE identities ADD COLUMN origin text NOT NULL DEFAULT '';
ALTER TABLE identities DROP CONSTRAINT identities_pkey;
ALTER TABLE identities ADD PRIMARY KEY (provider, origin, subject);

-- A session records the origin it signed in through; a session whose
-- sign-in has since moved to another origin is no longer honoured.
ALTER TABLE sessions ADD COLUMN provider_origin text NOT NULL DEFAULT '';

-- A membership is held per source, so a forge refresh and an accepted invite
-- never overwrite each other; the effective role is the highest across an
-- account's sources on a tenant.
ALTER TABLE memberships DROP CONSTRAINT memberships_pkey;
ALTER TABLE memberships ADD PRIMARY KEY (tenant_id, account_id, source);
