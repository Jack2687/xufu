package platform

import (
	"context"
	"github.com/jackc/pgx/v5"
	"net/http"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func priceFor(ctx context.Context, q rowQuerier, userID string, standard int64) (int64, error) {
	m, e := membershipFor(ctx, q, userID)
	if e != nil {
		return 0, e
	}
	return feeFor(m, standard), nil
}
func (a *App) quote(w http.ResponseWriter, r *http.Request) {
	var standard int64
	if a.DB.QueryRow(r.Context(), "SELECT fee FROM products WHERE id=$1 AND enabled", r.URL.Query().Get("product_id")).Scan(&standard) != nil {
		fail(w, 404, "服务不可用")
		return
	}
	m, e := membershipFor(r.Context(), a.DB, current(r).ID)
	if e != nil {
		fail(w, 500, "报价暂不可用")
		return
	}
	out(w, 200, M{"fee": feeFor(m, standard), "standard": standard, "membership": m, "payment_connected": a.subscriptionEnabled(r.Context())})
}
