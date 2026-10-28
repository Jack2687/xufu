package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"xupay/internal/cslive"
)

func (r *protocolRun) claim(ttl int) error {
	tag, err := r.a.DB.Exec(r.ctx, `UPDATE subscription_protocol SET lease_token=$2,lease_until=now()+interval '60 seconds'
 WHERE order_id=$1 AND expires_at>now() AND (lease_until IS NULL OR lease_until<now())
 AND EXISTS(SELECT 1 FROM payment_runs p WHERE p.order_id=$1 AND p.attempt_id=$3 AND p.state='preparing' AND NOT p.submitted)
 AND EXISTS(SELECT 1 FROM jobs j WHERE j.order_id=$1 AND j.lease_token=$2 AND j.state='running' AND j.lease_until>now())`, r.id, r.lease, r.attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	_, err = r.a.DB.Exec(r.ctx, "UPDATE payment_runs SET expires_at=now()+($3*interval '1 second') WHERE order_id=$1 AND attempt_id=$2", r.id, r.attempt, ttl)
	return err
}

func (r *protocolRun) heartbeat(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.job != nil && r.a.Heartbeat(ctx, r.job) != nil {
				cancel()
				return
			}
			tag, err := r.a.DB.Exec(ctx, "UPDATE subscription_protocol SET lease_until=now()+interval '60 seconds' WHERE order_id=$1 AND lease_token=$2 AND lease_until>now()", r.id, r.lease)
			if err != nil || tag.RowsAffected() != 1 {
				cancel()
				return
			}
			if r.job == nil {
				if _, err = r.a.DB.Exec(ctx, "UPDATE proxy_leases SET expires_at=now()+interval '75 seconds' WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1) AND expires_at>now()", r.id); err != nil {
					cancel()
					return
				}
			}
		}
	}
}

func (r *protocolRun) lock(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", r.id); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_protocol s JOIN payment_runs p ON p.order_id=s.order_id
 WHERE s.order_id=$1 AND s.lease_token=$2 AND s.lease_until>now() AND p.attempt_id=$3
 AND p.state NOT IN ('failed','cancelled'))`, r.id, r.lease, r.attempt).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	return nil
}

func (r *protocolRun) save(cp cslive.Checkpoint) error {
	if cp.AccountID != r.account || !strings.EqualFold(cp.Email, r.email) {
		return cslive.Failure("account_mismatch")
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	sealed, err := r.a.encrypt(string(raw))
	if err != nil {
		return err
	}
	tx, err := r.a.DB.Begin(r.ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(r.ctx)
	if err = r.lock(r.ctx, tx); err != nil {
		return err
	}
	_, err = tx.Exec(r.ctx, "UPDATE subscription_protocol SET checkpoint_enc=$3,phase=$4,updated_at=now() WHERE order_id=$1 AND lease_token=$2", r.id, r.lease, sealed, cp.Phase)
	if err != nil {
		return err
	}
	credentials, err := json.Marshal(checkoutInput{AccessToken: cp.AccessToken, SessionToken: cp.SessionToken, Cookies: cp.Cookies, AccountID: cp.AccountID, Email: cp.Email})
	if err != nil {
		return err
	}
	credentialEnc, err := r.a.encrypt(string(credentials))
	if err != nil {
		return err
	}
	if _, err = tx.Exec(r.ctx, "UPDATE subscription_runs SET credentials_enc=$2,cancel_started=cancel_started OR $3,updated_at=now() WHERE order_id=$1", r.id, credentialEnc, cp.Phase == "cancellation_requested"); err != nil {
		return err
	}
	if cp.Activation == "active" {
		if _, err = tx.Exec(r.ctx, "UPDATE orders SET activation='active',updated_at=now() WHERE id=$1", r.id); err != nil {
			return err
		}
	}
	if cp.Phase != r.last.Phase {
		phase := protocolDiagnosticPhase("", cp.Phase)
		if cp.Phase == "activation" || cp.Phase == "cancellation" {
			if err = paymentTraceTx(r.ctx, tx, r.id, phase, "running", ""); err != nil {
				return err
			}
		}
	}
	if link := str(cp.Metadata, "hosted_url"); validCheckoutURL(link) {
		linkEnc, err := r.a.encrypt(link)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(r.ctx, `INSERT INTO order_checkout_links(order_id,round,url_enc,generated_at,expires_at) VALUES($1,1,$2,now(),now()+interval '30 minutes') ON CONFLICT DO NOTHING`, r.id, linkEnc); err != nil {
			return err
		}
	}
	if err = tx.Commit(r.ctx); err != nil {
		return err
	}
	r.last = cp
	if cp.Payment == "charged" {
		event := protocolPaymentEvent(cp)
		event.ProtocolLease = r.lease
		if err = r.a.finishPayment(r.ctx, r.id, r.attempt, event); err != nil {
			return err
		}
	}
	return nil
}

func (r *protocolRun) approve(amount int64, currency string) (bool, error) {
	if amount < 0 || amount > r.maxAmount || strings.ToLower(currency) != r.currency {
		return false, cslive.Failure("checkout_amount_changed")
	}
	if r.a.config(r.ctx, "payment")["enabled"] != true {
		return false, cslive.Failure("payment_cancelled")
	}
	tx, err := r.a.DB.Begin(r.ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(r.ctx)
	if err = r.lock(r.ctx, tx); err != nil {
		return false, err
	}
	tag, err := tx.Exec(r.ctx, `UPDATE payment_runs SET state='submitting',submitted=true,amount=$3,currency=$4,updated_at=now()
 WHERE order_id=$1 AND attempt_id=$2 AND state='preparing' AND NOT submitted`, r.id, r.attempt, amount, strings.ToLower(currency))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrLeaseLost
	}
	if err = paymentTraceTx(r.ctx, tx, r.id, "pricing", "completed", ""); err != nil {
		return false, err
	}
	if err = paymentEventTx(r.ctx, tx, r.id, "payment_submitting", "confirming"); err != nil {
		return false, err
	}
	err = tx.Commit(r.ctx)
	return err == nil, err
}

func protocolPaymentEvent(cp cslive.Checkpoint) paymentEvent {
	e := paymentEvent{Type: "result", State: "unknown", Code: "result_unconfirmed", Currency: strings.ToLower(cp.Currency), Sent: cp.Payment != "not_attempted"}
	if cp.Amount != nil {
		e.Amount = *cp.Amount
	}
	switch cp.Payment {
	case "charged":
		e.State, e.Code = "succeeded", "succeeded"
		if charged := cp.ChargedPrice(); charged != nil {
			e.Amount = charged.AmountMinor
			e.Currency = strings.ToLower(charged.Currency)
		}
		e.Evidence = &paymentEvidence{CheckoutID: cp.CheckoutID, IntentID: cp.PaymentIntentID, Status: "succeeded", Currency: e.Currency}
		if cp.ChargedPrice() != nil {
			amount := e.Amount
			e.Evidence.AmountReceived = &amount
		}
	case "not_charged":
		e.State, e.Code = "declined", "payment_declined"
	case "action_required":
		e.State, e.Code = "action_required", "verification_required"
	case "authorized":
		e.State, e.Code = "review", "payment_authorized"
	case "not_attempted":
		e.State, e.Code = "failed", "preparation_failed"
	}
	return e
}

func (r *protocolRun) fail(err error) {
	code := cslive.ErrorCode(err)
	if errors.Is(err, ErrLeaseLost) {
		return
	}
	r.finish(cslive.Result{Checkpoint: r.last, ErrorCode: code})
}

func (r *protocolRun) finish(result cslive.Result) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 10*time.Second)
	defer cancel()
	cp := result.Checkpoint
	// Persist the returned snapshot even if the request deadline fired between
	// an upstream response and its checkpoint callback. Lease fencing still applies.
	if cp.AccountID != "" {
		finalizer := *r
		finalizer.ctx = ctx
		if err := finalizer.save(cp); err != nil {
			return
		}
	}
	if result.ErrorCode != "" || result.WarningCode != "" {
		code := result.ErrorCode
		if code == "" {
			code = result.WarningCode
		}
		r.a.saveProviderDiagnostic(providerDiagnostic{Order: r.id, Phase: protocolDiagnosticPhase("", cp.Phase), Code: protocolPublicCode(code), At: time.Now().UTC(), Details: M{"protocol_code": code, "provider_code": result.ProviderCode, "decline_code": result.DeclineCode}})
	}
	if cp.Payment == "" {
		cp.Payment = "not_attempted"
	}
	event := protocolPaymentEvent(cp)
	event.ProtocolLease = r.lease
	if err := r.a.finishPayment(ctx, r.id, r.attempt, event); err != nil {
		return
	}
	tx, err := r.a.DB.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); err != nil {
		return
	}
	if err = r.lock(ctx, tx); err != nil {
		// A final no-charge result has already made the payment terminal.
		var lease string
		if tx.QueryRow(ctx, "SELECT COALESCE(lease_token,'') FROM subscription_protocol WHERE order_id=$1 AND lease_until>now()", r.id).Scan(&lease) != nil || lease != r.lease {
			return
		}
	}
	var payment string
	if tx.QueryRow(ctx, "SELECT state FROM payment_runs WHERE order_id=$1", r.id).Scan(&payment) != nil {
		return
	}
	activation, renewal, status := "pending", "pending", "review"
	jobState := "review"
	clear := false
	if payment == "succeeded" {
		status, jobState = "succeeded", "completed"
		activation = "review"
		if cp.Activation == "active" {
			activation = "active"
		}
		renewal = "review"
		if r.keep {
			renewal = "kept"
		} else if activation == "active" && cp.Renewal == "off" {
			renewal = "disabled"
		}
		clear = activation == "active" && (renewal == "disabled" || renewal == "kept")
	} else if payment == "failed" || payment == "cancelled" {
		status, jobState = payment, "failed"
		clear = true
	}
	if _, err = tx.Exec(ctx, `UPDATE subscription_protocol SET checkpoint_enc=CASE WHEN $3 THEN '' ELSE checkpoint_enc END,
 lease_token=NULL,lease_until=NULL,updated_at=now() WHERE order_id=$1 AND lease_token=$2`, r.id, r.lease, clear); err != nil {
		return
	}
	if payment == "succeeded" {
		if _, err = tx.Exec(ctx, "UPDATE orders SET status=$2,activation=$3,renewal=$4,updated_at=now() WHERE id=$1", r.id, status, activation, renewal); err != nil {
			return
		}
		if _, err = tx.Exec(ctx, "UPDATE subscription_runs SET renewal_state=$2,credentials_enc='',updated_at=now() WHERE order_id=$1", r.id, renewal); err != nil {
			return
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE jobs SET state=$2,lease_token=NULL,lease_until=NULL,updated_at=now() WHERE order_id=$1", r.id, jobState); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, "DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)", r.id); err != nil {
		return
	}
	traceState := "review"
	if clear {
		traceState = "completed"
	}
	if jobState == "failed" {
		traceState = "failed"
	}
	if err = paymentTraceTx(ctx, tx, r.id, "complete", traceState, protocolPublicCode(result.ErrorCode, result.WarningCode)); err != nil {
		return
	}
	if payment == "succeeded" {
		activationState, renewalState := "review", "review"
		if activation == "active" {
			activationState = "completed"
		}
		if renewal == "disabled" || renewal == "kept" {
			renewalState = "completed"
		}
		if err = paymentTraceTx(ctx, tx, r.id, "activation", activationState, ""); err != nil {
			return
		}
		if err = paymentTraceTx(ctx, tx, r.id, "renewal", renewalState, protocolPublicCode(renewal)); err != nil {
			return
		}
		message := "付款已成功，订阅或续费状态需要继续核实；请勿重新付款。"
		if clear {
			if r.keep {
				message = "订阅已生效，按您的选择保留自动续费。"
			} else {
				message = "订阅已生效，自动续费已关闭。"
			}
		}
		if _, err = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'renewal',$2)", r.id, message); err != nil {
			return
		}
	}
	_ = tx.Commit(ctx)
}
