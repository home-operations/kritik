-- Dashboard roles come from sign-in (ADR-0014 §2.5): the sign-in's role
-- mapping and the forge decide them once, and the session carries the
-- result. Memberships and invites have nothing left to hold.
DROP TABLE invites;
DROP TABLE memberships;

-- Sessions from before hold no grant; they end, and their owners sign in
-- again.
DELETE FROM sessions;
-- role is the instance role. A member reads every account when
-- all_accounts is set, and otherwise the forge accounts in accounts,
-- lowercased as "<forge>/<name>". grant_key fingerprints the sign-in
-- configuration that decided the grant, so a session outlives no change
-- to it.
ALTER TABLE sessions
    ADD COLUMN role         text    NOT NULL CHECK (role IN ('admin', 'member')),
    ADD COLUMN all_accounts boolean NOT NULL,
    ADD COLUMN accounts     text[]  NOT NULL,
    ADD COLUMN grant_key    text    NOT NULL;
