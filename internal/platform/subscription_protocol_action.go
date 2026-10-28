package platform

import "xupay/internal/cslive"

// The CLI's Pay -> continuePaymentAction -> Recover sequence lives inside the
// order worker. The server opens the original SDK page and starts its button;
// a customer/admin HTTP request is not needed to advance this branch.
func (r *protocolRun) continuePaymentAction(client protocolPaymentClient, prior cslive.Result, v *paymentAction) (cslive.Result, error) {
	cp := prior.Checkpoint
	if cp.Payment != "action_required" {
		return prior, nil
	}
	if err := r.save(cp); err != nil {
		return prior, err
	}
	tx, err := r.a.DB.Begin(r.ctx)
	if err != nil {
		return prior, err
	}
	defer tx.Rollback(r.ctx)
	if err = r.lock(r.ctx, tx); err != nil {
		return prior, err
	}
	if _, err = tx.Exec(r.ctx, "UPDATE payment_runs SET state='checking',error_code='checking',updated_at=now() WHERE order_id=$1 AND attempt_id=$2 AND state<>'succeeded'", r.id, r.attempt); err != nil {
		return prior, err
	}
	if err = tx.Commit(r.ctx); err != nil {
		return prior, err
	}
	if v == nil {
		// Automatic runs do not publish an interactive session or capture frames.
		v = &paymentAction{automatic: true, state: "preparing"}
	}
	if err = r.runPaymentAction(cp, v); err != nil {
		// Match the package: a lost/expired verification session preserves the
		// original payment. Never restart Pay or fabricate a successful callback.
		prior.WarningCode = cslive.ErrorCode(err)
		return prior, nil
	}
	cp.Proxy = client.Proxy()
	r.a.paymentTrace(r.ctx, r.id, "checking", "running", "", 0)
	// One SDK pass only. Recover may still report action_required; leave that
	// evidence intact instead of repeatedly invoking the same challenge.
	return client.Recover(r.ctx, cp)
}
