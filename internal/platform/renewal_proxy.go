package platform

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Resolve only the explicitly selected global proxy. Never use the regional
// checkout pool, environment proxy or a direct transport for renewal operations.
func (a *App) openRenewalClient(ctx context.Context) (*http.Client, func(), error) {
	// Private, in-process provider seam used exclusively by tests.
	if a.renewalClient != nil {
		c := a.renewalClient()
		return c, c.CloseIdleConnections, nil
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return nil, nil, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
		return nil, nil, e
	}
	var id, enc string
	e = tx.QueryRow(ctx, `SELECT r.id,r.endpoint_enc FROM resources r JOIN settings s ON s.key='proxy' AND r.id=s.value->>'global_id'
 WHERE r.enabled AND r.kind='proxy' AND (SELECT count(*) FROM proxy_leases l WHERE l.resource_id=r.id AND l.expires_at>now())<r.capacity`).Scan(&id, &enc)
	if e == pgx.ErrNoRows {
		return nil, nil, ErrProxyUnavailable
	}
	if e != nil {
		return nil, nil, e
	}
	endpoint, e := a.decrypt(enc)
	if e != nil {
		return nil, nil, e
	}
	factory := newPaymentClient
	if a.renewalProxyClient != nil {
		factory = a.renewalProxyClient
	}
	client, closeTransport, e := factory(endpoint, 30*time.Second)
	if e != nil {
		return nil, nil, e
	}
	lease := uid()
	if _, e = tx.Exec(ctx, "INSERT INTO proxy_leases(id,resource_id,expires_at) VALUES($1,$2,now()+interval '110 seconds')", lease, id); e != nil {
		closeTransport()
		return nil, nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		closeTransport()
		return nil, nil, e
	}
	return client, func() {
		closeTransport()
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.releaseProxy(release, lease)
	}, nil
}

func (a *App) waitRenewalProxy(ctx context.Context, id, lease string, standalone bool) {
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	delay := num(a.config(save, "payment"), "renewal_check_interval_seconds", 30)
	if standalone {
		a.DB.Exec(save, `UPDATE renewal_tasks SET status='queued',message='等待办理资源，稍后自动重试。',checks=GREATEST(checks-1,0),next_at=now()+($3*interval '1 second'),lease_token=NULL,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2`, id, lease, delay)
	} else {
		a.DB.Exec(save, `UPDATE subscription_runs SET checks=GREATEST(checks-1,0),next_at=now()+($3*interval '1 second'),lease_token=NULL,lease_until=NULL,updated_at=now() WHERE order_id=$1 AND lease_token=$2`, id, lease, delay)
	}
}

func (a *App) setGlobalProxy(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !read(w, r, &body) {
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
			return nil, e
		}
		var enabled bool
		if e := tx.QueryRow(r.Context(), "SELECT enabled FROM resources WHERE id=$1 AND kind='proxy'", r.PathValue("id")).Scan(&enabled); e != nil || !enabled {
			return nil, bad("全局代理不存在或未启用")
		}
		if _, e := tx.Exec(r.Context(), "UPDATE settings SET value=jsonb_set(value,'{global_id}',to_jsonb($1::text)) WHERE key='proxy'", r.PathValue("id")); e != nil {
			return nil, e
		}
		return M{"ok": true}, a.audit(r.Context(), tx, r, "proxy.global", r.PathValue("id"), "设置续费全局代理")
	})
}
