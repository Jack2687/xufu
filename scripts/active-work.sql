-- Read-only release guard. A nonzero result means do not restart the app.
-- psql: compatible with the schema before the first protocol upgrade.
\set ON_ERROR_STOP on
\set protocol_active 0
\set protocol_idle 0
\set legacy_idle 0
SELECT to_regclass('subscription_protocol') IS NOT NULL AS has_protocol \gset
\if :has_protocol
SELECT count(*) AS protocol_active FROM subscription_protocol WHERE lease_until>now() \gset
-- Durable paused protocol orders have no executing worker. Retain their encrypted
-- checkpoint, held funds and original proxy; these are safe to carry across a restart.
-- Incomplete records, leases and any executing jobs still block release.
SELECT count(*) AS protocol_idle FROM subscription_runs u
 JOIN subscription_protocol s ON s.order_id=u.order_id
 JOIN orders o ON o.id=u.order_id JOIN payment_runs p ON p.order_id=u.order_id
 JOIN jobs j ON j.order_id=u.order_id
 WHERE u.credentials_enc<>'' AND u.lease_token IS NULL
 AND s.checkpoint_enc<>'' AND s.lease_token IS NULL AND s.lease_until IS NULL
 AND o.status='review' AND p.state IN ('review','action_required') AND p.submitted
 AND j.state='review' AND j.lease_token IS NULL AND j.lease_until IS NULL \gset
-- A finished legacy browser can also leave a durable, read-only review. Wait
-- past its runtime deadline and require the original checkout and no workers.
-- Keeping authorization for later verification does not make it active work.
SELECT count(*) AS legacy_idle FROM subscription_runs u
 JOIN orders o ON o.id=u.order_id JOIN payment_runs p ON p.order_id=u.order_id
 JOIN jobs j ON j.order_id=u.order_id
 WHERE u.credentials_enc<>'' AND u.lease_token IS NULL AND u.lease_until IS NULL
 AND u.renewal_state='not_started' AND o.status='review'
 AND p.state IN ('review','action_required') AND p.submitted
 AND p.hosted_url_enc<>'' AND p.expires_at<now()
 AND j.state='completed' AND j.lease_token IS NULL AND j.lease_until IS NULL
 AND NOT EXISTS(SELECT 1 FROM subscription_protocol s WHERE s.order_id=u.order_id) \gset
\endif
SELECT
 (SELECT count(*) FROM jobs WHERE state IN ('queued','running')) +
 (SELECT count(*) FROM orders WHERE status IN ('queued','processing')) +
 (SELECT count(*) FROM payment_runs WHERE state IN ('preparing','quoted','submitting','checking')) +
 (SELECT count(*) FROM subscription_runs WHERE credentials_enc<>'' OR lease_token IS NOT NULL) +
 :protocol_active - :protocol_idle - :legacy_idle +
 (SELECT count(*) FROM renewal_tasks WHERE status IN ('queued','processing','checking') OR lease_token IS NOT NULL) +
 (SELECT count(*) FROM proxy_checks WHERE state IN ('queued','running')) AS active_work;
