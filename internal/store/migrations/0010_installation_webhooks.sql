-- When an installation's webhook last delivered a request kritik verified,
-- so the dashboard can tell an installation whose forge sends webhooks from
-- one kritik only polls. The listener writes it at most once a minute.
ALTER TABLE installations ADD COLUMN last_webhook_at timestamptz;
