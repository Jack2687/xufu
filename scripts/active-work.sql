-- Read-only release guard. A nonzero result means do not restart the app.
SELECT
 (SELECT count(*) FROM jobs WHERE state IN ('queued','running')) +
 (SELECT count(*) FROM orders WHERE status IN ('queued','processing')) +
 (SELECT count(*) FROM payment_runs WHERE state IN ('preparing','quoted','submitting','checking')) +
 (SELECT count(*) FROM subscription_runs WHERE credentials_enc<>'' OR lease_token IS NOT NULL) +
 (SELECT count(*) FROM renewal_tasks WHERE status IN ('queued','processing','checking') OR lease_token IS NOT NULL) +
 (SELECT count(*) FROM proxy_checks WHERE state IN ('queued','running')) AS active_work;
