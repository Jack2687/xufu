package platform

import (
	"errors"
	"time"

	"xupay/internal/cslive"
)

func (r *protocolRun) prepareLegacyCheckout(in checkoutInput) (*cslive.PreparedCheckout, error) {
	var enc string
	var expires time.Time
	err := r.a.DB.QueryRow(r.ctx, `SELECT s.endpoint_enc,s.expires_at FROM order_proxy_sessions s
 JOIN resources p ON p.id=s.resource_id WHERE s.order_id=$1 AND p.enabled`, r.id).Scan(&enc, &expires)
	if err != nil || !expires.After(time.Now()) {
		return nil, cslive.Failure("proxy_session_unavailable")
	}
	if !in.TokenExpiresAt.IsZero() && !in.TokenExpiresAt.After(time.Now()) {
		return nil, cslive.Failure("invalid_session")
	}
	endpoint, err := r.a.decrypt(enc)
	if err != nil {
		return nil, cslive.Failure("proxy_session_unavailable")
	}
	in.Mode = "long"
	if in.CheckoutType == "oaics" {
		in.Mode = "short"
	}
	in.ObserveIP = nil
	started := time.Now()
	r.a.paymentTrace(r.ctx, r.id, "checkout", "running", "", 0)
	result, err := r.a.runCheckoutFlow(r.ctx, r.job, endpoint, in, subscriptionFlowLegacy, "subscription")
	if err != nil {
		var failure checkoutError
		code := "preparation_failed"
		if errors.As(err, &failure) {
			code = failure.Code
			if failure.Unknown {
				code = "checkout_result_unknown"
			}
		}
		r.a.paymentTrace(r.ctx, r.id, "checkout", "failed", code, time.Since(started))
		return nil, cslive.Failure(code)
	}
	sealed, err := r.a.encrypt(result.URL)
	if err != nil {
		return nil, err
	}
	tx, err := r.a.DB.Begin(r.ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.ctx)
	if err = r.lock(r.ctx, tx); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.ctx, "UPDATE checkout_runs SET hosted_url_enc=$2,link_expires_at=now()+interval '30 minutes' WHERE order_id=$1", r.id, sealed); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.ctx, `INSERT INTO order_checkout_links(order_id,round,url_enc,generated_at,expires_at)
 VALUES($1,1,$2,now(),now()+interval '30 minutes') ON CONFLICT DO NOTHING`, r.id, sealed); err != nil {
		return nil, err
	}
	if err = tx.Commit(r.ctx); err != nil {
		return nil, err
	}
	r.a.paymentTrace(r.ctx, r.id, "checkout", "completed", "", time.Since(started))
	return &cslive.PreparedCheckout{CheckoutID: result.CheckoutID, URL: result.URL, ProcessorEntity: result.ProcessorEntity,
		PublishableKey: result.PublishableKey, ClientSecret: result.ClientSecret, ReturnURL: result.ReturnURL}, nil
}
