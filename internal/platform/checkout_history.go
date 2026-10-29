package platform

import (
	"context"
	"encoding/json"
	"regexp"
	"xupay/internal/cslive"
)

var protocolCheckoutID = regexp.MustCompile(`^(?:cs_live_|oaics_)[A-Za-z0-9]+$`)
var protocolCheckoutEntity = regexp.MustCompile(`^[a-z0-9_]{2,64}$`)

// Same custom URL rule as the supplied package's checkoutURL. Never fabricate
// a Hosted fragment, use a return URL, or make another upstream request.
func protocolCheckoutURL(cp cslive.Checkpoint) string {
	if raw := str(cp.Metadata, "hosted_url"); validCheckoutURL(raw) {
		return raw
	}
	if cp.Metadata["checkout_ui_mode"] == "custom" && protocolCheckoutID.MatchString(cp.CheckoutID) && protocolCheckoutEntity.MatchString(cp.Entity) {
		return "https://chatgpt.com/checkout/" + cp.Entity + "/" + cp.CheckoutID
	}
	return ""
}

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
	// Existing protocol orders still retain their encrypted checkpoint. Recover
	// the display URL without inventing a past generation timestamp or retrying.
	if len(result) == 0 {
		var enc string
		if a.DB.QueryRow(ctx, "SELECT checkpoint_enc FROM subscription_protocol WHERE order_id=$1", id).Scan(&enc) == nil {
			if raw, err := a.decrypt(enc); err == nil {
				var cp cslive.Checkpoint
				if json.Unmarshal([]byte(raw), &cp) == nil {
					if link := protocolCheckoutURL(cp); link != "" {
						result = append(result, M{"round": 1, "available": true, "url": link, "generated_at": nil, "source": "checkpoint"})
					}
				}
			}
		}
	}
	return result, nil
}
