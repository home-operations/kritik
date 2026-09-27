-- A GitHub App is installed once per account, so kritik finds the
-- installation from the repository it works on rather than keeping one id
-- per configured installation.
ALTER TABLE installations DROP COLUMN external_id;
