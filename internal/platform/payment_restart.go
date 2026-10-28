package platform

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
)

// Checkout can also lose a sticky session while waiting for a safe retry. Only
// a fenced job that has never attempted its upstream write may rotate it.
func (a *App) restartCheckoutNetworkTx(ctx context.Context, tx pgx.Tx, id, code string) (bool, error) {
	var protocol bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM subscription_protocol WHERE order_id=$1)", id).Scan(&protocol); err != nil || protocol {
		return false, err
	}
	if code != "proxy_session_changed" && code != "proxy_session_expired" {
		return false, nil
	}
	var cfg M
	if e := tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='payment'").Scan(&cfg); e != nil {
		return false, e
	}
	var eligible bool
	if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders o JOIN payment_runs p ON p.order_id=o.id JOIN jobs j ON j.order_id=o.id JOIN checkout_runs c ON c.order_id=o.id
 WHERE o.id=$1 AND o.workflow='subscription' AND p.state='preparing' AND NOT p.submitted AND p.fee_state='held' AND p.network_restarts<$2
 AND j.state IN ('queued','running') AND NOT j.side_effect_started AND c.credentials_enc<>'' AND c.credentials_expires_at>now()+interval '1 minute')`, id, num(cfg, "network_restart_limit", 2)).Scan(&eligible); e != nil {
		return false, e
	}
	if !eligible {
		return false, nil
	}
	if e := paymentTraceTx(ctx, tx, id, "checkout", "failed", code); e != nil {
		return false, e
	}
	for _, q := range []string{
		"DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)",
		"DELETE FROM order_proxy_sessions WHERE order_id=$1",
		"UPDATE checkout_runs SET hosted_url_enc='',link_expires_at=NULL,proxy_lease_id=NULL,resource_id=NULL WHERE order_id=$1",
		"UPDATE payment_runs SET network_restarts=network_restarts+1,error_code='network_restarted',updated_at=now() WHERE order_id=$1",
		"UPDATE jobs SET state='queued',attempts=0,next_at=now(),lease_token=NULL,lease_until=NULL,error_code='network_restarted',updated_at=now() WHERE order_id=$1",
		"UPDATE orders SET status='queued',updated_at=now() WHERE id=$1",
	} {
		if _, e := tx.Exec(ctx, q, id); e != nil {
			return false, e
		}
	}
	if e := paymentTraceTx(ctx, tx, id, "restart", "completed", code); e != nil {
		return false, e
	}
	return true, nil
}

// Only a positively known no-submit runtime result may rebuild checkout. Lost
// responses, process exits, checking, successful and uncertain writes never do.
func (a *App) restartPaymentNetwork(ctx context.Context, id, attempt string, event paymentEvent) (bool, error) {
	if event.Type != "result" || event.State != "failed" || event.Sent {
		return false, nil
	}
	switch event.Code {
	case "proxy_session_changed", "proxy_session_expired", "proxy_session_unavailable":
	default:
		return false, nil
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		return false, e
	}
	var state, workflow, credentials, job, fee string
	var restarts int
	var expiry time.Time
	e = tx.QueryRow(ctx, `SELECT p.state,p.network_restarts,p.fee_state,o.workflow,c.credentials_enc,c.credentials_expires_at,j.state
 FROM payment_runs p JOIN orders o ON o.id=p.order_id JOIN checkout_runs c ON c.order_id=o.id JOIN jobs j ON j.order_id=o.id
 WHERE o.id=$1 AND p.attempt_id=$2 FOR UPDATE OF p,j`, id, attempt).Scan(&state, &restarts, &fee, &workflow, &credentials, &expiry, &job)
	if e != nil {
		return false, e
	}
	cfg := a.config(ctx, "payment")
	if workflow != "subscription" || job != "completed" || fee != "held" || credentials == "" || !expiry.After(time.Now().Add(time.Minute)) || restarts >= num(cfg, "network_restart_limit", 2) {
		return false, nil
	}
	if state != "preparing" && state != "quoted" && state != "submitting" {
		return false, nil
	}
	if e = paymentTraceTx(ctx, tx, id, "result", "failed", event.Code); e != nil {
		return false, e
	}
	ttl := num(cfg, "subscription_wait_seconds", 600) + num(cfg, "prepare_timeout_seconds", 120) + num(cfg, "result_timeout_seconds", 120) + 60
	for _, q := range []struct {
		s    string
		args []any
	}{
		{"DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)", []any{id}},
		{"DELETE FROM order_proxy_sessions WHERE order_id=$1", []any{id}},
		{"UPDATE checkout_runs SET hosted_url_enc='',link_expires_at=NULL,proxy_lease_id=NULL,resource_id=NULL WHERE order_id=$1", []any{id}},
		{"UPDATE jobs SET state='queued',attempts=0,next_at=now(),lease_token=NULL,lease_until=NULL,side_effect_started=false,error_code='network_restarted',updated_at=now() WHERE order_id=$1", []any{id}},
		{"UPDATE payment_runs SET state='preparing',network_restarts=network_restarts+1,submitted=false,amount=NULL,currency='',quote_expires_at=NULL,hosted_url_enc='',error_code='network_restarted',expires_at=now()+($2*interval '1 second'),updated_at=now() WHERE order_id=$1", []any{id, ttl}},
		{"UPDATE orders SET status='queued',updated_at=now() WHERE id=$1", []any{id}},
		{"INSERT INTO order_events(order_id,stage,message) VALUES($1,'network_restarted','本轮未提交扣款，正在重新准备办理网络与结账信息；服务费保持原冻结，不会重复收取。')", []any{id}},
	} {
		if _, e = tx.Exec(ctx, q.s, q.args...); e != nil {
			return false, e
		}
	}
	if e = paymentTraceTx(ctx, tx, id, "restart", "completed", event.Code); e != nil {
		return false, e
	}
	if e = tx.Commit(ctx); e != nil {
		return false, e
	}
	return true, nil
}
