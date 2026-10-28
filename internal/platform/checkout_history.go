package platform

import "context"

// Call only from the administrator detail projection. Customer order APIs and
// the execution path must not consume history or use it to repeat a payment.
func (a *App) adminCheckoutLinks(ctx context.Context, id string) ([]M, error) {
	rows, err := a.query(ctx, "SELECT round,url_enc,generated_at,expires_at FROM order_checkout_links WHERE order_id=$1 ORDER BY round", id)
	if err != nil {
		return nil, err
	}
	result := make([]M, 0, len(rows))
	for _, row := range rows {
		item := M{"round": row["round"], "generated_at": row["generated_at"], "expires_at": row["expires_at"], "available": false}
		if raw, e := a.decrypt(str(row, "url_enc")); e == nil && validCheckoutURL(raw) {
			item["url"], item["available"] = raw, true
		}
		result = append(result, item)
	}
	return result, nil
}
