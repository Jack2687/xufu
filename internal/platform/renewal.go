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
	var enc string
	var expiry time.Time
	var started bool
	var checks int
	e := a.DB.QueryRow(ctx, "SELECT credentials_enc,credentials_expires_at,cancel_started,checks FROM subscription_runs WHERE order_id=$1 AND lease_token=$2", id, lease).Scan(&enc, &expiry, &started, &checks)
	if e != nil {
		return
	}
	cfg := a.config(ctx, "payment")
	state, message := "pending", "正在核实自动续费设置。"
	if enc == "" || !expiry.After(time.Now()) {
		state, message = "review", "订阅成功，取消自动续费失败：账号授权已到期。请在续费管理粘贴新的 Session 继续处理，无需重新付款。"
	} else {
		raw, e := a.decrypt(enc)
		var in checkoutInput
		if e != nil || json.Unmarshal([]byte(raw), &in) != nil || in.AccessToken == "" || in.AccountID == "" {
			state, message = "review", "订阅成功，取消自动续费失败。请在续费管理粘贴新的 Session 继续处理，无需重新付款。"
		} else {
			client, closeClient, err := a.openRenewalClient(ctx)
			if err != nil {
				a.waitRenewalProxy(ctx, id, lease, false)
				return
			}
			defer closeClient()
			active, renew, err := renewalStatus(ctx, client, in)
			if err == nil && active && !renew {
				state, message = "disabled", "自动续费已关闭，当前订阅有效期保留。"
			} else if err == nil && active && renew && !started {
				// Persist the fence BEFORE the only cancellation write, including on restart.
				tag, claimErr := a.DB.Exec(ctx, "UPDATE subscription_runs SET cancel_started=true WHERE order_id=$1 AND lease_token=$2 AND NOT cancel_started AND lease_until>now()", id, lease)
				if claimErr == nil && tag.RowsAffected() == 1 {
					cancelRenewal(ctx, client, in)
					// A timeout or 200 response is not success; read the current setting.
					active, renew, err = renewalStatus(ctx, client, in)
					if err == nil && active && !renew {
						state, message = "disabled", "自动续费已关闭，当前订阅有效期保留。"
					}
				}
			}
			if renewalAuthorizationRejected(err) {
				state, message = "review", subscriptionRenewalAuthorizationMessage
			}
			in = checkoutInput{}
			raw = ""
		}
	}
	if state == "pending" && checks >= num(cfg, "renewal_check_limit", 10) {
		state, message = "review", "订阅成功，取消自动续费失败：暂未确认关闭结果。请在续费管理粘贴新的 Session 继续核实，无需重新付款。"
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
	tag, e := tx.Exec(ctx, `UPDATE subscription_runs SET renewal_state=$3,credentials_enc=CASE WHEN $3 IN ('disabled','review') THEN '' ELSE credentials_enc END,next_at=now()+($4*interval '1 second'),lease_token=NULL,lease_until=NULL,updated_at=now() WHERE order_id=$1 AND lease_token=$2`, id, lease, state, num(cfg, "renewal_check_interval_seconds", 30))
	if e != nil || tag.RowsAffected() != 1 {
		return
	}
	status := "processing"
	if state == "disabled" {
		status = "succeeded"
	}
	if state == "review" {
		status = "succeeded"
	}
	if _, e = tx.Exec(ctx, "UPDATE orders SET renewal=$2,status=$3,updated_at=now() WHERE id=$1", id, state, status); e != nil {
		return
	}
	if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'renewal',$2)", id, message); e != nil {
		return
	}
	tx.Commit(ctx)
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
