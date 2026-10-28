package platform

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/url"
	"path"
	"regexp"
)

var paymentIntentID = regexp.MustCompile(`^pi_[A-Za-z0-9]{1,200}$`)

func (a *App) savePaymentEvidence(ctx context.Context, tx pgx.Tx, id, attempt string, event paymentEvent) error {
	e := event.Evidence
	if e == nil {
		return nil
	} // Older runtimes/test seams have no direct intent evidence.
	var enc string
	if err := tx.QueryRow(ctx, "SELECT hosted_url_enc FROM payment_runs WHERE order_id=$1 AND attempt_id=$2", id, attempt).Scan(&enc); err != nil {
		return err
	}
	raw, err := a.decrypt(enc)
	if err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || !hostedPaymentURL(raw) || path.Base(u.Path) != e.CheckoutID || e.Currency != event.Currency {
		return checkoutFailure("invalid_response", false, false)
	}
	if e.IntentID != "" && !paymentIntentID.MatchString(e.IntentID) {
		return checkoutFailure("invalid_response", false, false)
	}
	if e.Status != "succeeded" && e.Status != "pending" && e.Status != "authorized" && e.Status != "action_required" {
		return checkoutFailure("invalid_response", false, false)
	}
	if e.AmountReceived != nil && (*e.AmountReceived <= 0 || *e.AmountReceived > 10000000000 || e.IntentID == "" || e.Status != "succeeded" || *e.AmountReceived != event.Amount) {
		return checkoutFailure("invalid_response", false, false)
	}
	data, _ := json.Marshal(e)
	enc, err = a.encrypt(string(data))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE payment_runs SET payment_evidence_enc=$3,charged_amount=COALESCE($4,charged_amount),charged_currency=CASE WHEN $4::bigint IS NOT NULL THEN $5 ELSE charged_currency END WHERE order_id=$1 AND attempt_id=$2`, id, attempt, enc, e.AmountReceived, e.Currency)
	return err
}
