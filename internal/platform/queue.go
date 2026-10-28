package platform

// The queue is durable infrastructure for the future payment adapter. No HTTP
// endpoint can enqueue or execute a payment while that adapter is disconnected.
import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type Job struct {
	ID, OrderID, UserID, Lease string
	Kind                       string
	Attempts                   int
	SideEffect                 bool
}

var ErrLeaseLost = errors.New("job lease expired or superseded")

func (a *App) Claim(ctx context.Context) (*Job, error) {
	return a.claim(ctx, "")
}
func (a *App) claim(ctx context.Context, kind string) (*Job, error) {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return nil, e
	}
	var cfg M
	if e = tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='scheduler'").Scan(&cfg); e != nil {
		return nil, e
	}
	if cfg["paused"] == true {
		return nil, nil
	}
	limit := num(cfg, "max_parallel", 2)
	perUser := num(cfg, "per_user_parallel", 1)
	// Never retry an expired lease with uncertain upstream side effects.
	rows, e := tx.Query(ctx, "UPDATE jobs SET state=CASE WHEN side_effect_started THEN 'review' WHEN attempts>=max_attempts THEN 'failed' ELSE 'queued' END,error_code='lease_expired',lease_token=NULL,lease_until=NULL,next_at=now()+($1*interval '1 second'),updated_at=now() WHERE state='running' AND lease_until<now() RETURNING order_id,state", num(cfg, "retry_delay_seconds", 30))
	if e != nil {
		return nil, e
	}
	states := [][2]string{}
	for rows.Next() {
		var id, state string
		if e = rows.Scan(&id, &state); e != nil {
			rows.Close()
			return nil, e
		}
		states = append(states, [2]string{id, state})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for _, s := range states {
		if _, e = tx.Exec(ctx, "UPDATE orders SET status=$2,updated_at=now() WHERE id=$1", s[0], s[1]); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)", s[0]); e != nil {
			return nil, e
		}
		if s[1] != "queued" {
			if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc='' WHERE order_id=$1", s[0]); e != nil {
				return nil, e
			}
		}
		if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) SELECT $1,$2,$3 WHERE EXISTS(SELECT 1 FROM checkout_runs WHERE order_id=$1)", s[0], s[1], map[string]string{"review": "准备任务中断，链接创建结果尚未确认，已停止重复提交。", "failed": "准备任务超时且已达到重试上限，未提交付款。", "queued": "准备任务中断，将在配置的重试间隔后重新检查。"}[s[1]]); e != nil {
			return nil, e
		}
	}
	var running int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE state='running'").Scan(&running); e != nil {
		return nil, e
	}
	if running >= limit {
		if e = tx.Commit(ctx); e != nil {
			return nil, e
		}
		return nil, nil
	}
	j := &Job{}
	e = tx.QueryRow(ctx, `SELECT j.id,j.order_id,j.user_id,j.attempts,j.kind FROM jobs j WHERE j.state='queued' AND j.next_at<=now() AND j.attempts<j.max_attempts AND ($2='' OR j.kind=$2) AND (SELECT count(*) FROM jobs running WHERE running.state='running' AND running.user_id=j.user_id)<$1 ORDER BY j.created_at,j.id LIMIT 1 FOR UPDATE SKIP LOCKED`, perUser, kind).Scan(&j.ID, &j.OrderID, &j.UserID, &j.Attempts, &j.Kind)
	if e == pgx.ErrNoRows {
		if e = tx.Commit(ctx); e != nil {
			return nil, e
		}
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if j.Kind == "checkout" {
		var country string
		var expiry time.Time
		if e = tx.QueryRow(ctx, "SELECT o.country,c.credentials_expires_at FROM orders o JOIN checkout_runs c ON c.order_id=o.id WHERE o.id=$1", j.OrderID).Scan(&country, &expiry); e != nil {
			return nil, e
		}
		if !expiry.After(time.Now()) {
			if _, e = tx.Exec(ctx, "UPDATE jobs SET state='failed',error_code='invalid_session',updated_at=now() WHERE id=$1", j.ID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, "UPDATE orders SET status='failed',updated_at=now() WHERE id=$1", j.OrderID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc='' WHERE order_id=$1", j.OrderID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'failed','排队期间 Session 已到期，请获取新 Session 后重新申请。')", j.OrderID); e != nil {
				return nil, e
			}
			return nil, tx.Commit(ctx)
		}
		p, err := a.acquireOrderProxyTx(ctx, tx, j.OrderID, country, time.Duration(num(cfg, "task_timeout_seconds", 180)+15)*time.Second)
		if errors.Is(err, ErrProxyUnavailable) {
			if _, e = tx.Exec(ctx, `INSERT INTO payment_trace_events(order_id,phase,state,code,round)
 SELECT $1,'queue','waiting','waiting_proxy',COALESCE((SELECT network_restarts+1 FROM payment_runs WHERE order_id=$1),1)
 WHERE EXISTS(SELECT 1 FROM jobs WHERE id=$2 AND error_code<>'waiting_proxy')`, j.OrderID, j.ID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, "UPDATE jobs SET next_at=now()+($2*interval '1 second'),error_code='waiting_proxy' WHERE id=$1", j.ID, num(cfg, "retry_delay_seconds", 30)); e != nil {
				return nil, e
			}
			return nil, tx.Commit(ctx)
		}
		if err != nil {
			var failure checkoutError
			if errors.As(err, &failure) {
				if restarted, restartErr := a.restartCheckoutNetworkTx(ctx, tx, j.OrderID, failure.Code); restartErr != nil {
					return nil, restartErr
				} else if restarted {
					return nil, tx.Commit(ctx)
				}
				if e = paymentTraceTx(ctx, tx, j.OrderID, "proxy", "failed", failure.Code); e != nil {
					return nil, e
				}
				if _, e = tx.Exec(ctx, "UPDATE jobs SET state='failed',error_code=$2,updated_at=now() WHERE id=$1", j.ID, failure.Code); e != nil {
					return nil, e
				}
				if _, e = tx.Exec(ctx, "UPDATE orders SET status='failed',updated_at=now() WHERE id=$1", j.OrderID); e != nil {
					return nil, e
				}
				if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc='' WHERE order_id=$1", j.OrderID); e != nil {
					return nil, e
				}
				if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'failed','本次办理网络会话已失效，未继续付款，请重新申请。')", j.OrderID); e != nil {
					return nil, e
				}
				return nil, tx.Commit(ctx)
			}
			return nil, err
		}
		if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET resource_id=$2,proxy_lease_id=$3 WHERE order_id=$1", j.OrderID, p.ID, p.Lease); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) SELECT $1,'processing',CASE WHEN workflow='subscription' THEN '办理资源已就绪，正在准备订阅。' ELSE '已分配代理，正在准备结账链接。' END FROM orders WHERE id=$1", j.OrderID); e != nil {
			return nil, e
		}
	}
	j.Lease = secret()
	j.Attempts++
	_, e = tx.Exec(ctx, "UPDATE jobs SET state='running',attempts=attempts+1,lease_token=$2,lease_until=now()+($3*interval '1 second'),updated_at=now() WHERE id=$1", j.ID, j.Lease, num(cfg, "task_timeout_seconds", 180))
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE orders SET status='processing',updated_at=now() WHERE id=$1", j.OrderID); e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	if j.Kind == "checkout" {
		a.paymentTrace(ctx, j.OrderID, "proxy", "completed", "", 0)
	}
	return j, nil
}
func (a *App) BeforeSideEffect(ctx context.Context, j *Job) error {
	tag, e := a.DB.Exec(ctx, "UPDATE jobs SET side_effect_started=true,updated_at=now() WHERE id=$1 AND lease_token=$2 AND state='running' AND lease_until>now()", j.ID, j.Lease)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}
func (a *App) Heartbeat(ctx context.Context, j *Job) error {
	tag, e := a.DB.Exec(ctx, "UPDATE jobs SET lease_until=now()+($3*interval '1 second'),updated_at=now() WHERE id=$1 AND lease_token=$2 AND state='running' AND lease_until>now()", j.ID, j.Lease, num(a.config(ctx, "scheduler"), "task_timeout_seconds", 180))
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if j.Kind == "checkout" {
		tag, e = a.DB.Exec(ctx, "UPDATE proxy_leases SET expires_at=now()+($2*interval '1 second') WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1) AND expires_at>now()", j.OrderID, num(a.config(ctx, "scheduler"), "task_timeout_seconds", 180)+15)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
	}
	return nil
}
func (a *App) Finish(ctx context.Context, j *Job, successful, safeRetry bool, code string) error {
	// Public messages are an allowlist, never raw exceptions or upstream bodies.
	messages := map[string]string{"network": "连接暂时不可用，等待安全重试。", "invalid_credentials": "账号授权需要更新。", "payment_declined": "付款未通过，请核对付款方式。", "unknown_result": "处理结果尚未确认，已转入核实。", "done": "本轮处理已确认完成。"}
	msg, ok := messages[code]
	if !ok {
		code = "unknown_result"
		msg = messages[code]
		safeRetry = false
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var side bool
	var attempts, max int
	e = tx.QueryRow(ctx, "SELECT side_effect_started,attempts,max_attempts FROM jobs WHERE id=$1 AND lease_token=$2 AND state='running' AND lease_until>now() FOR UPDATE", j.ID, j.Lease).Scan(&side, &attempts, &max)
	if e == pgx.ErrNoRows {
		return ErrLeaseLost
	}
	if e != nil {
		return e
	}
	state := "failed"
	orderState := "failed"
	if successful {
		state = "completed"
		orderState = "succeeded"
		code = "done"
		msg = messages[code]
	} else if side {
		state = "review"
		orderState = "review"
		code = "unknown_result"
		msg = messages[code]
	} else if safeRetry && attempts < max {
		state = "queued"
		orderState = "queued"
	}
	delay := time.Duration(num(a.config(ctx, "scheduler"), "retry_delay_seconds", 30)) * time.Second
	if _, e = tx.Exec(ctx, "UPDATE jobs SET state=$3,error_code=$4,next_at=$5,lease_until=NULL,lease_token=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2", j.ID, j.Lease, state, code, time.Now().Add(delay)); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE orders SET status=$2,updated_at=now() WHERE id=$1", j.OrderID, orderState); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,$2,$3)", j.OrderID, orderState, msg); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
