package platform

import (
	"context"
	"time"
)

func (a *App) Maintain(ctx context.Context) error {
	days := num(a.config(ctx, "risk"), "event_retention_days", 90)
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "DELETE FROM proxy_checks WHERE state='completed' AND updated_at<now()-interval '7 days'"); e != nil {
		return e
	}
	for _, q := range []string{"DELETE FROM sessions WHERE expires_at<now()", "DELETE FROM rate_buckets WHERE expires_at<now()-interval '1 hour'", "DELETE FROM blocked_ips WHERE expires_at<now()-interval '1 day'"} {
		if _, e = tx.Exec(ctx, q); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, `UPDATE checkout_runs c SET credentials_enc='' WHERE c.credentials_expires_at<now() OR
 (c.order_id IN (SELECT order_id FROM jobs WHERE state IN ('failed','review','completed')) AND NOT EXISTS
 (SELECT 1 FROM orders o JOIN payment_runs p ON p.order_id=o.id WHERE o.id=c.order_id AND o.workflow='subscription' AND p.state IN ('preparing','quoted','submitting','checking')))`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET hosted_url_enc='' WHERE link_expires_at<now()"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE subscription_runs SET credentials_enc='' WHERE credentials_expires_at<now()"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE renewal_tasks SET credentials_enc='' WHERE credentials_expires_at<now()"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM proxy_leases WHERE expires_at<now()-interval '1 hour'"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE order_proxy_sessions SET endpoint_enc='' WHERE expires_at<now()"); e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "DELETE FROM risk_events WHERE id IN (SELECT id FROM risk_events WHERE created_at<now()-($1*interval '1 day') ORDER BY id LIMIT 10000)", days)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) RunMaintenance(ctx context.Context) {
	a.Maintain(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.Maintain(ctx)
		}
	}
}
