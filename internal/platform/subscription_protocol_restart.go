package platform

import (
	"context"
	"encoding/json"
	"xupay/internal/cslive"
)

func (a *App) recoverProtocolRuns(ctx context.Context) error {
	rows, err := a.query(ctx, `SELECT s.order_id,s.checkpoint_enc,p.attempt_id,p.submitted,o.email,o.country,o.keep_renewal,u.currency
 FROM subscription_protocol s JOIN payment_runs p ON p.order_id=s.order_id JOIN orders o ON o.id=s.order_id
 JOIN subscription_runs u ON u.order_id=s.order_id WHERE (s.lease_token IS NOT NULL AND s.lease_until<now())
 OR (s.lease_token IS NULL AND p.state IN ('preparing','quoted','submitting','checking') AND p.expires_at<now()) LIMIT 100`)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id, lease := str(row, "order_id"), secret()
		tag, err := a.DB.Exec(ctx, `UPDATE subscription_protocol SET lease_token=$2,lease_until=now()+interval '60 seconds'
 WHERE order_id=$1 AND ((lease_token IS NOT NULL AND lease_until<now()) OR (lease_token IS NULL AND EXISTS(
 SELECT 1 FROM payment_runs p WHERE p.order_id=$1 AND p.state IN ('preparing','quoted','submitting','checking') AND p.expires_at<now())))`, id, lease)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			continue
		}
		var cp cslive.Checkpoint
		if row["submitted"] == true {
			cp.Payment = "unknown"
		}
		if enc := str(row, "checkpoint_enc"); enc != "" {
			raw, decodeErr := a.decrypt(enc)
			if decodeErr != nil || json.Unmarshal([]byte(raw), &cp) != nil {
				cp.Payment = "unknown"
			}
		}
		run := &protocolRun{a: a, ctx: ctx, id: id, attempt: str(row, "attempt_id"), lease: lease, account: cp.AccountID, email: str(row, "email"), country: str(row, "country"), keep: row["keep_renewal"] == true, currency: str(row, "currency"), last: cp}
		run.finish(cslive.Result{Checkpoint: cp, ErrorCode: "result_unconfirmed"})
	}
	return nil
}
