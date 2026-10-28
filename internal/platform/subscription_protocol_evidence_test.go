package platform

import (
	"testing"
	"xupay/internal/cslive"
)

func TestNativeRecoveryAddsCapturedAmountWithoutResettling(t *testing.T) {
	h, r, cp := nativeFixture(t, true)
	if ok, err := r.approve(1500, "PHP"); err != nil || !ok {
		t.Fatal(err)
	}
	cp.Payment, cp.Phase = "charged", "payment_result"
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	var amount *int64
	h.a.DB.QueryRow(r.ctx, "SELECT charged_amount FROM payment_runs WHERE order_id=$1", r.id).Scan(&amount)
	if amount != nil {
		t.Fatal("estimate counted as captured amount")
	}
	cp = nativeCharged(cp, 1400)
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	h.a.DB.QueryRow(r.ctx, "SELECT charged_amount FROM payment_runs WHERE order_id=$1", r.id).Scan(&amount)
	if amount == nil || *amount != 1400 {
		t.Fatal("recovery did not add captured amount")
	}
	paymentBalances(t, h, 90000, 0)
	cp.Activation, cp.Renewal, cp.Phase = "active", "on", "completed"
	cp.CancelRequested = false
	r.finish(cslive.Result{Checkpoint: cp})
}
