-- Gitea installations (ADR-0003 amendment): Gitea speaks the same API,
-- webhooks and OAuth as Forgejo, so it is a new installations.forge value
-- routed to the existing Forgejo client rather than a schema change beyond
-- this constraint.
ALTER TABLE installations DROP CONSTRAINT installations_forge_check;
ALTER TABLE installations ADD CONSTRAINT installations_forge_check
    CHECK (forge IN ('github', 'gitlab', 'forgejo', 'gitea'));
