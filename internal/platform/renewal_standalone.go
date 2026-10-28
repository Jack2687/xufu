package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const renewalFields = "id,email,status,message,created_at,updated_at"

func (a *App) renewalTasks(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, "SELECT "+renewalFields+" FROM renewal_tasks WHERE user_id=$1 ORDER BY created_at DESC LIMIT 200", current(r).ID)
}

func (a *App) renewalTask(w http.ResponseWriter, r *http.Request) {
	rows, e := a.query(r.Context(), "SELECT "+renewalFields+" FROM renewal_tasks WHERE id=$1 AND user_id=$2", r.PathValue("id"), current(r).ID)
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	if len(rows) == 0 {
		fail(w, 404, "记录不存在")
		return
	}
	events, e := a.query(r.Context(), "SELECT status,message,created_at FROM renewal_events WHERE task_id=$1 ORDER BY id", r.PathValue("id"))
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	out(w, 200, M{"task": rows[0], "events": events})
}

func (a *App) createRenewalTask(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Session  json.RawMessage `json:"session"`
		Accepted bool            `json:"accepted"`
	}
	if !read(w, r, &v) {
		return
	}
	if !v.Accepted {
		fail(w, 400, "请确认关闭自动续费")
		return
	}
	info, e := inspectSession(v.Session)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 120 {
		fail(w, 400, "请提供 8～120 位 Idempotency-Key")
		return
	}
	ctx, u := r.Context(), current(r)
	cfg := a.config(ctx, "risk")
	if !a.limit(ctx, "renewal-create:"+u.ID, num(cfg, "order_per_minute", 5)) {
		a.event(r, "renewal_rate", "medium", "续费申请过于频繁")
		fail(w, 429, "提交过于频繁，请稍后再试")
		return
	}
	var quoted string
	if json.Unmarshal(v.Session, &quoted) == nil {
		v.Session = []byte(quoted)
	}
	var s struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(v.Session, &s) != nil {
		fail(w, 400, "Session 格式不正确")
		return
	}
	raw, _ := json.Marshal(checkoutInput{AccessToken: s.AccessToken, AccountID: info.AccountID})
	enc, e := a.encrypt(string(raw))
	if e != nil {
		fail(w, 500, "暂时无法受理，请稍后重试")
		return
	}
	expiry := info.ExpiresAt
	if cap := time.Now().Add(24 * time.Hour); expiry.After(cap) {
		expiry = cap
	}
	accountHash := hash(info.AccountID)
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		// Shared admission lock prevents a subscription and cancellation for one account racing.
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
			return nil, e
		}
		var id, oldHash, status string
		e := tx.QueryRow(ctx, `SELECT k.task_id,k.account_hash,t.status FROM renewal_request_keys k JOIN renewal_tasks t ON t.id=k.task_id WHERE k.user_id=$1 AND k.key=$2`, u.ID, key).Scan(&id, &oldHash, &status)
		if e == nil {
			if oldHash != accountHash {
				return nil, businessError{409, "同一请求标识不能用于不同账号"}
			}
			return M{"id": id, "status": status, "replayed": true}, nil
		}
		if e != pgx.ErrNoRows {
			return nil, e
		}
		var count int
		recovery, e := renewalRecoveryTx(ctx, tx, u.ID, info.Email, accountHash)
		if e != nil {
			return nil, e
		}
		var owner string
		e = tx.QueryRow(ctx, "SELECT id,user_id,status FROM renewal_tasks WHERE account_hash=$1 AND (status IN ('queued','processing','checking','review') OR id=$2) FOR UPDATE", accountHash, recovery.taskID).Scan(&id, &owner, &status)
		replayed := e == nil
		if e != nil && e != pgx.ErrNoRows {
			return nil, e
		}
		if recovery.taskID != "" && (!replayed || id != recovery.taskID) {
			return nil, businessError{409, "原续费记录需由运营核实，请勿重复提交"}
		}
		if replayed {
			if owner != u.ID {
				return nil, businessError{409, "该账号已有未结束的续费申请"}
			}
			if len(recovery.orders) > 0 && status != "review" && status != "no_subscription" {
				// An already-running continuation must not have its fence changed.
				for _, orderID := range recovery.orders {
					var linked bool
					if e = tx.QueryRow(ctx, "SELECT COALESCE(renewal_task_id=$2,false) FROM subscription_runs WHERE order_id=$1", orderID, id).Scan(&linked); e != nil {
						return nil, e
					}
					if !linked {
						return nil, businessError{409, "该账号正在处理续费，请等待当前任务结束"}
					}
				}
			}
			if status == "review" || status == "no_subscription" {
				// Fresh authorization resumes verification; the previous write fence is permanent.
				if _, e = tx.Exec(ctx, "UPDATE renewal_tasks SET credentials_enc=$2,credentials_expires_at=$3,cancel_started=cancel_started OR $4,status='queued',checks=0,next_at=now(),updated_at=now(),message='已更新授权，等待继续核实。' WHERE id=$1", id, enc, expiry, recovery.started); e != nil {
					return nil, e
				}
				status = "queued"
				if _, e = tx.Exec(ctx, "INSERT INTO renewal_events(task_id,status,message) VALUES($1,'queued','已更新授权，等待继续核实。')", id); e != nil {
					return nil, e
				}
			}
		} else {
			if e = tx.QueryRow(ctx, "SELECT count(*) FROM renewal_tasks WHERE user_id=$1 AND status IN ('queued','processing','checking','review')", u.ID).Scan(&count); e != nil {
				return nil, e
			}
			if count >= num(cfg, "max_open_orders", 10) {
				return nil, bad("未结束的续费申请已达上限，请先处理原申请")
			}
			id, status = uid(), "queued"
			if _, e = tx.Exec(ctx, "INSERT INTO renewal_tasks(id,user_id,email,account_hash,credentials_enc,credentials_expires_at,cancel_started) VALUES($1,$2,$3,$4,$5,$6,$7)", id, u.ID, info.Email, accountHash, enc, expiry, recovery.started); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO renewal_events(task_id,status,message) VALUES($1,'queued','申请已受理，等待处理；不收取服务费。')", id); e != nil {
				return nil, e
			}
		}
		if e = recovery.attach(ctx, tx, id, accountHash); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO renewal_request_keys(user_id,key,account_hash,task_id) VALUES($1,$2,$3,$4)", u.ID, key, accountHash, id); e != nil {
			return nil, e
		}
		if e = a.audit(ctx, tx, r, "renewal_request", id, "提交独立关闭续费申请"); e != nil {
			return nil, e
		}
		return M{"id": id, "status": status, "replayed": replayed}, nil
	})
}

// Both renewal queues share the same bounded worker capacity.
const renewalLeaseCount = `SELECT (SELECT count(*) FROM subscription_runs WHERE lease_until>now())+(SELECT count(*) FROM renewal_tasks WHERE lease_until>now())`

func (a *App) claimStandaloneRenewal(ctx context.Context) (string, string, error) {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886004)"); e != nil {
		return "", "", e
	}
	cfg := a.config(ctx, "payment")
	var n int
	if e = tx.QueryRow(ctx, renewalLeaseCount).Scan(&n); e != nil {
		return "", "", e
	}
	if n >= num(cfg, "max_parallel", 2) {
		return "", "", nil
	}
	var id string
	e = tx.QueryRow(ctx, `SELECT t.id FROM renewal_tasks t WHERE t.status IN ('queued','processing','checking') AND t.next_at<=now() AND (t.lease_until IS NULL OR t.lease_until<now())
 AND (SELECT count(*) FROM renewal_tasks x WHERE x.user_id=t.user_id AND x.lease_until>now())<$1
 ORDER BY t.next_at,t.created_at LIMIT 1 FOR UPDATE OF t SKIP LOCKED`, num(cfg, "per_user_parallel", 1)).Scan(&id)
	if e == pgx.ErrNoRows {
		return "", "", nil
	}
	if e != nil {
		return "", "", e
	}
	lease := secret()
	if _, e = tx.Exec(ctx, "UPDATE renewal_tasks SET status='processing',message='正在检查续费设置。',lease_token=$2,lease_until=now()+interval '110 seconds',checks=checks+1,updated_at=now() WHERE id=$1", id, lease); e != nil {
		return "", "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO renewal_events(task_id,status,message) VALUES($1,'processing','正在检查续费设置。')", id); e != nil {
		return "", "", e
	}
	return id, lease, tx.Commit(ctx)
}

func (a *App) runStandaloneRenewal(ctx context.Context, id, lease string) {
	var enc string
	var expiry time.Time
	var started bool
	var checks int
	if e := a.DB.QueryRow(ctx, "SELECT credentials_enc,credentials_expires_at,cancel_started,checks FROM renewal_tasks WHERE id=$1 AND lease_token=$2", id, lease).Scan(&enc, &expiry, &started, &checks); e != nil {
		return
	}
	cfg := a.config(ctx, "payment")
	state, message := "checking", "正在核实续费设置，稍后自动更新。"
	if enc == "" || !expiry.After(time.Now()) {
		state, message = "review", "授权已到期，请粘贴新的 Session 继续核实。"
	} else {
		raw, e := a.decrypt(enc)
		var in checkoutInput
		if e != nil || json.Unmarshal([]byte(raw), &in) != nil || in.AccessToken == "" || in.AccountID == "" {
			state, message = "review", "账号授权不可用，请粘贴新的 Session 继续核实。"
		} else {
			client, closeClient, err := a.openRenewalClient(ctx)
			if err != nil {
				a.waitRenewalProxy(ctx, id, lease, true)
				return
			}
			defer closeClient()
			active, renew, err := renewalStatus(ctx, client, in)
			if err == nil && active && renew && !started {
				tag, e := a.DB.Exec(ctx, "UPDATE renewal_tasks SET cancel_started=true,status='checking',message='正在确认关闭结果。',updated_at=now() WHERE id=$1 AND lease_token=$2 AND NOT cancel_started AND lease_until>now()", id, lease)
				if e != nil || tag.RowsAffected() != 1 {
					return
				}
				cancelRenewal(ctx, client, in)
				active, renew, err = renewalStatus(ctx, client, in)
			}
			if err == nil && !renew {
				if active {
					state, message = "disabled", "已确认自动续费关闭，当前订阅有效期保留。"
				} else {
					state, message = "no_subscription", "未发现需要续费的有效订阅，无需关闭。"
				}
			}
			if renewalAuthorizationRejected(err) {
				state, message = "review", standaloneRenewalAuthorizationMessage
			}
			in = checkoutInput{}
			raw = ""
		}
	}
	if state == "checking" && checks >= num(cfg, "renewal_check_limit", 10) {
		state, message = "review", "暂时无法确认结果，已暂停自动核实。可粘贴新的 Session 继续核实。"
	}
	// Persist the outcome even if the network deadline fired, while honoring the lease.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, e := a.DB.Begin(save)
	if e != nil {
		return
	}
	defer tx.Rollback(save)
	if _, e = tx.Exec(save, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return
	}
	tag, e := tx.Exec(save, `UPDATE renewal_tasks SET status=$3,message=$4,credentials_enc=CASE WHEN $3 IN ('disabled','no_subscription','review') THEN '' ELSE credentials_enc END,
 next_at=now()+($5*interval '1 second'),lease_token=NULL,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2`, id, lease, state, message, num(cfg, "renewal_check_interval_seconds", 30))
	if e != nil || tag.RowsAffected() != 1 {
		return
	}
	if _, e = tx.Exec(save, "INSERT INTO renewal_events(task_id,status,message) VALUES($1,$2,$3)", id, state, message); e != nil {
		return
	}
	if e = syncRenewalRecoveryTx(save, tx, id, state); e != nil {
		return
	}
	tx.Commit(save)
}

func cancelRenewal(ctx context.Context, client *http.Client, in checkoutInput) {
	h := http.Header{"Authorization": {"Bearer " + in.AccessToken}, "Chatgpt-Account-Id": {in.AccountID}, "Content-Type": {"application/json"}, "Accept": {"application/json"}, "User-Agent": {webCheckoutUA}, "Origin": {"https://chatgpt.com"}, "Referer": {"https://chatgpt.com/"}}
	payload, _ := json.Marshal(M{"account_id": in.AccountID})
	providerJSON(ctx, client, "POST", subscriptionEndpoint+"/cancel", h, payload, true)
}
