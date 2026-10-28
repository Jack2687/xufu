package platform

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/url"
	"time"
)

// Private provider operations. The caller only receives business states.
const subscriptionEndpoint = "https://chatgpt.com/backend-api/subscriptions"

// A rejected credential cannot recover by polling the same token. Keep public
// messages free of provider endpoints, raw responses and authorization data.
func renewalAuthorizationRejected(err error) bool {
	var failure checkoutError
	return errors.As(err, &failure) && failure.Code == "invalid_session"
}

const subscriptionRenewalAuthorizationMessage = "订阅成功，取消自动续费失败：账号登录授权已失效。请在续费管理粘贴该账号的新 Session 继续处理，无需重新付款。"
const standaloneRenewalAuthorizationMessage = "账号登录授权已失效，已暂停核实。请粘贴新的 Session 继续处理。"

func renewalStatus(ctx context.Context, c *http.Client, in checkoutInput) (active, renew bool, err error) {
	h := http.Header{"Authorization": {"Bearer " + in.AccessToken}, "Chatgpt-Account-Id": {in.AccountID}, "Accept": {"application/json"}, "User-Agent": {webCheckoutUA}, "Origin": {"https://chatgpt.com"}, "Referer": {"https://chatgpt.com/"}}
	v, e := providerJSON(ctx, c, "GET", subscriptionEndpoint+"?account_id="+url.QueryEscape(in.AccountID), h, nil, false)
	if e != nil {
		return false, false, e
	}
	a, ok := v["is_active"].(bool)
	if !ok {
		return false, false, checkoutFailure("invalid_response", false, false)
	}
	// `id` in this response is the subscription ID, not the workspace ID.
	if id := str(v, "account_id"); id != "" && id != in.AccountID {
		return false, false, checkoutFailure("invalid_response", false, false)
	}
	r, ok := v["will_renew"].(bool)
	if !ok {
		return false, false, checkoutFailure("invalid_response", false, false)
	}
	return a, r, nil
}

func (a *App) runRenewal(ctx context.Context, id, lease string) {
	startedAt := time.Now()
	var enc, email, product, activation string
	var expiry time.Time
	var started, keep bool
	var checks, activationChecks int
	e := a.DB.QueryRow(ctx, `SELECT s.credentials_enc,s.credentials_expires_at,s.cancel_started,s.checks,s.activation_checks,o.email,o.product_id,o.keep_renewal,o.activation
 FROM subscription_runs s JOIN orders o ON o.id=s.order_id WHERE s.order_id=$1 AND s.lease_token=$2`, id, lease).Scan(&enc, &expiry, &started, &checks, &activationChecks, &email, &product, &keep, &activation)
	if e != nil {
		return
	}
	cfg := a.config(ctx, "payment")
	state, message := "pending", "付款已成功，正在核实订阅生效。"
	previousActivation := activation
	if activation == "not_checked" {
		activation = "pending"
	}
	var resultErr error
	if enc == "" || !expiry.After(time.Now()) {
		resultErr = checkoutFailure("invalid_session", false, false)
	} else {
		raw, err := a.decrypt(enc)
		var in checkoutInput
		if err != nil || json.Unmarshal([]byte(raw), &in) != nil || in.AccessToken == "" || in.AccountID == "" {
			resultErr = checkoutFailure("invalid_session", false, false)
		} else {
			if in.Email == "" {
				in.Email = email
			}
			client, closeClient, err := a.openOrderRenewalClient(ctx, id)
			if waitingRenewalProxy(err) {
				a.paymentTrace(ctx, id, "activation", "waiting", "proxy_unavailable", time.Since(startedAt))
				a.waitRenewalProxy(ctx, id, lease, false)
				return
			}
			resultErr = err
			if err == nil {
				defer closeClient()
				auth := &providerSession{ctx: a.providerContext(ctx, id, "session_refresh"), client: client, in: &in, purpose: "activation",
					persist: func(v checkoutInput) error { return a.saveRenewalCredentials(ctx, id, lease, v, false) },
					trace:   func(state, code string) { a.paymentTrace(ctx, id, "session_refresh", state, code, 0) }}
				resultErr = auth.prepare()
				if resultErr == nil {
					a.paymentTrace(ctx, id, "activation", "running", "", 0)
					var snapshot subscriptionSnapshot
					snapshot, resultErr = readSubscription(a.providerContext(ctx, id, "activation"), auth)
					if resultErr == nil && snapshot.Active {
						// New orders require the purchased plan; legacy rows retain prior semantics.
						if previousActivation != "not_checked" {
							resultErr = verifySubscriptionPlan(a.providerContext(ctx, id, "activation"), auth, snapshot, product)
						}
						if resultErr == nil {
							activation = "active"
							if previousActivation != "active" {
								checks = 1
							}
							a.paymentTrace(ctx, id, "activation", "completed", "", time.Since(startedAt))
							if keep {
								state, message = "kept", "订阅已生效，按您的选择保留自动续费。"
							} else {
								message = "订阅已生效，正在核实自动续费设置。"
								a.paymentTrace(ctx, id, "renewal", "running", "", 0)
								// Cancellation is a separate authorization boundary: refresh again
								// even when activation just recovered from a 401, then re-read.
								auth.purpose = "renewal"
								if in.SessionToken != "" || in.Cookies != "" {
									resultErr = auth.refresh()
									if resultErr == nil {
										snapshot, resultErr = readSubscription(a.providerContext(ctx, id, "renewal"), auth)
									}
									if resultErr == nil && snapshot.Active && previousActivation != "not_checked" {
										resultErr = verifySubscriptionPlan(a.providerContext(ctx, id, "activation"), auth, snapshot, product)
									}
								} else {
									// Legacy AT-only input cannot manufacture refresh credentials.
									resultErr = auth.prepare()
								}
								if resultErr == nil && snapshot.Active && snapshot.RenewalKnown && !snapshot.Renew {
									state, message = "disabled", "自动续费已关闭，当前订阅有效期保留。"
								} else if resultErr == nil && snapshot.Active && snapshot.RenewalKnown && snapshot.Renew && !started {
									tag, claimErr := a.DB.Exec(ctx, "UPDATE subscription_runs SET cancel_started=true WHERE order_id=$1 AND lease_token=$2 AND NOT cancel_started AND lease_until>now()", id, lease)
									if claimErr != nil || tag.RowsAffected() != 1 {
										return
									}
									cancelRenewal(a.providerContext(ctx, id, "renewal"), client, in)
									snapshot, resultErr = readSubscription(a.providerContext(ctx, id, "renewal"), auth)
									if resultErr == nil && snapshot.Active && previousActivation != "not_checked" {
										resultErr = verifySubscriptionPlan(a.providerContext(ctx, id, "activation"), auth, snapshot, product)
									}
									if resultErr == nil && snapshot.Active && snapshot.RenewalKnown && !snapshot.Renew {
										state, message = "disabled", "自动续费已关闭，当前订阅有效期保留。"
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if resultErr != nil && (orderRenewalProxyTerminal(resultErr) || renewalAuthorizationRejected(resultErr) || providerCode(resultErr) == "account_mismatch" || providerCode(resultErr) == "plan_mismatch") {
		state = "review"
	}
	delay := num(cfg, "renewal_check_interval_seconds", 30)
	if activation != "active" {
		activationChecks++
		delay = num(cfg, "activation_check_interval_seconds", 5)
		if activationChecks >= num(cfg, "activation_check_limit", 12) {
			state = "review"
		}
		if state == "review" {
			activation = "review"
			message = "付款已成功，订阅生效暂未确认，请联系运营核实；无需再次付款。"
			if renewalAuthorizationRejected(resultErr) {
				message = "付款已成功，账号授权已失效，订阅生效暂未确认。请更新授权后核实，无需再次付款。"
			}
		} else {
			message = "付款已成功，等待订阅同步，系统将自动再次核实。"
		}
		code := "activation_pending"
		traceState := "waiting"
		if state == "review" {
			code = "activation_review"
			traceState = "review"
		}
		if resultErr != nil {
			code = providerCode(resultErr)
		}
		a.paymentTrace(ctx, id, "activation", traceState, code, time.Since(startedAt))
	} else if state == "pending" && checks >= num(cfg, "renewal_check_limit", 10) {
		state = "review"
	}
	if activation == "active" && state == "review" {
		message = "订阅成功，取消自动续费失败：暂未确认关闭结果。请在续费管理粘贴新的 Session 继续核实，无需重新付款。"
		if renewalAuthorizationRejected(resultErr) {
			message = subscriptionRenewalAuthorizationMessage
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ctx = save
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		return
	}
	tag, e := tx.Exec(ctx, `UPDATE subscription_runs SET renewal_state=$3,credentials_enc=CASE WHEN $3 IN ('disabled','kept','review') THEN '' ELSE credentials_enc END,
 next_at=now()+($4*interval '1 second'),lease_token=NULL,lease_until=NULL,checks=$5,activation_checks=$6,updated_at=now() WHERE order_id=$1 AND lease_token=$2 AND lease_until>now()`, id, lease, state, delay, checks, activationChecks)
	if e != nil || tag.RowsAffected() != 1 {
		return
	}
	status, renewal := "processing", state
	if state == "disabled" || state == "kept" || state == "review" {
		status = "succeeded"
	}
	if keep {
		renewal = "kept"
	}
	if _, e = tx.Exec(ctx, "UPDATE orders SET renewal=$2,status=$3,activation=$4,updated_at=now() WHERE id=$1", id, renewal, status, activation); e != nil {
		return
	}
	if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'renewal',$2)", id, message); e != nil {
		return
	}
	if state != "pending" {
		traceState := "completed"
		if state == "review" {
			traceState = "review"
		}
		if paymentTraceTx(ctx, tx, id, "complete", traceState, state) != nil {
			return
		}
	}
	if tx.Commit(ctx) == nil && activation == "active" {
		traceState := "completed"
		if state == "pending" {
			traceState = "waiting"
		} else if state == "review" {
			traceState = "review"
		}
		a.paymentTrace(ctx, id, "renewal", traceState, state, time.Since(startedAt))
	}
}

func (a *App) claimRenewal(ctx context.Context) (string, string, error) {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886004)"); e != nil {
		return "", "", e
	}
	var n int
	if e = tx.QueryRow(ctx, renewalLeaseCount).Scan(&n); e != nil {
		return "", "", e
	}
	if n >= num(a.config(ctx, "payment"), "max_parallel", 2) {
		return "", "", nil
	}
	var id string
	e = tx.QueryRow(ctx, `SELECT s.order_id FROM subscription_runs s JOIN payment_runs p ON p.order_id=s.order_id WHERE s.renewal_state='pending' AND p.state='succeeded' AND s.next_at<=now() AND (s.lease_until IS NULL OR s.lease_until<now()) ORDER BY s.next_at,s.order_id LIMIT 1 FOR UPDATE OF s SKIP LOCKED`).Scan(&id)
	if e == pgx.ErrNoRows {
		return "", "", nil
	}
	if e != nil {
		return "", "", e
	}
	lease := secret()
	_, e = tx.Exec(ctx, "UPDATE subscription_runs SET lease_token=$2,lease_until=now()+interval '110 seconds',checks=checks+1 WHERE order_id=$1", id, lease)
	if e != nil {
		return "", "", e
	}
	e = tx.Commit(ctx)
	return id, lease, e
}

func (a *App) RunRenewals(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	standaloneFirst := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Alternate admission priority so either queue makes progress at concurrency one.
			for _, standalone := range []bool{standaloneFirst, !standaloneFirst} {
				claim, run := a.claimRenewal, a.runRenewal
				if standalone {
					claim, run = a.claimStandaloneRenewal, a.runStandaloneRenewal
				}
				id, lease, e := claim(ctx)
				if e == nil && id != "" {
					go func() { c, cancel := context.WithTimeout(ctx, 100*time.Second); defer cancel(); run(c, id, lease) }()
				}
			}
			standaloneFirst = !standaloneFirst
		}
	}
}
