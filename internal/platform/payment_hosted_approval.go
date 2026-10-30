package platform

import (
	"context"
	"net/url"
	"path"
)

// Persist before letting the official merchant page approve the original charge.
// A timeout/restart never clears this fence and cannot submit the approval again.
func (a *App) authorizeHostedApproval(ctx context.Context, id, attempt string, event paymentEvent) error {
	if !a.paymentEnabled(ctx) || event.Evidence == nil || event.Evidence.Status != "pending" || event.Evidence.Currency != event.Currency || event.Amount <= 0 {
		return ErrLeaseLost
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); err != nil {
		return err
	}
	var rawEnc string
	err = tx.QueryRow(ctx, `SELECT p.hosted_url_enc FROM payment_runs p JOIN orders o ON o.id=p.order_id
 JOIN order_proxy_sessions s ON s.order_id=p.order_id JOIN resources r ON r.id=s.resource_id
 WHERE p.order_id=$1 AND p.attempt_id=$2 AND p.submitted AND NOT p.approval_started
 AND p.state IN ('submitting','checking') AND p.expires_at>now() AND p.amount=$3 AND p.currency=$4
 AND o.status IN ('processing','review') AND s.expires_at>now() AND s.endpoint_enc<>'' AND (o.workflow='subscription' OR s.expected_ip<>'') AND r.enabled
 AND NOT EXISTS(SELECT 1 FROM subscription_protocol c WHERE c.order_id=p.order_id) FOR UPDATE OF p`, id, attempt, event.Amount, event.Currency).Scan(&rawEnc)
	if err != nil {
		return err
	}
	raw, err := a.decrypt(rawEnc)
	if err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || !hostedPaymentURL(raw) || path.Base(u.Path) != event.Evidence.CheckoutID {
		return ErrLeaseLost
	}
	if _, err = tx.Exec(ctx, "UPDATE payment_runs SET approval_started=true,updated_at=now() WHERE order_id=$1", id); err != nil {
		return err
	}
	if err = paymentTraceTx(ctx, tx, id, "confirming", "running", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
