package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"xupay/internal/cslive"
)

func TestAdminCloseReleasesExactlyOnceAndFencesWorkers(t *testing.T) {
	for _, mode := range []string{"preparing", "action_required", "checking"} {
		t.Run(mode, func(t *testing.T) {
			h, r, cp := nativeFixture(t, false)
			if mode != "preparing" {
				if ok, e := r.approve(1500, "PHP"); !ok || e != nil {
					t.Fatal(e)
				}
				cp.Payment, cp.Phase = "action_required", "payment_result"
				if e := r.save(cp); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "checking" {
				h.a.DB.Exec(r.ctx, "UPDATE payment_runs SET state='checking' WHERE order_id=$1", r.id)
			}
			var checkpoint, proxy, billing string
			h.a.DB.QueryRow(r.ctx, `SELECT s.checkpoint_enc,p.endpoint_enc,b.details_enc FROM subscription_protocol s JOIN order_proxy_sessions p USING(order_id) JOIN order_billing b USING(order_id) WHERE order_id=$1`, r.id).Scan(&checkpoint, &proxy, &billing)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.a.paymentMu.Lock()
			h.a.payments[r.id] = &paymentSession{cancel: cancel}
			h.a.paymentActions = map[string]*paymentAction{r.id: {cancel: cancel, state: "verifying", frame: []byte{255, 216, 1}, expires: time.Now().Add(time.Minute)}}
			h.a.paymentMu.Unlock()
			path := "/api/admin/orders/" + r.id + "/close"
			body := M{"reason": "fixture operator decision", "accepted": true}
			expect(t, h.request("POST", path, body, h.member, nil), 403)
			expect(t, h.request("POST", path, M{"reason": "fixture"}, h.admin, nil), 400)
			var wg sync.WaitGroup
			for i := 0; i < 3; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); expect(t, h.request("POST", path, body, h.admin, nil), 200) }()
			}
			wg.Wait()
			if ctx.Err() == nil {
				t.Fatal("worker not cancelled")
			}
			paymentBalances(t, h, 100000, 0)
			var state, fee, code, status, afterCheckpoint, afterProxy, afterBilling string
			var submitted bool
			err := h.a.DB.QueryRow(r.ctx, `SELECT o.status,p.state,p.fee_state,p.error_code,p.submitted,s.checkpoint_enc,q.endpoint_enc,b.details_enc FROM orders o JOIN payment_runs p ON p.order_id=o.id JOIN subscription_protocol s ON s.order_id=o.id JOIN order_proxy_sessions q ON q.order_id=o.id JOIN order_billing b ON b.order_id=o.id WHERE o.id=$1`, r.id).Scan(&status, &state, &fee, &code, &submitted, &afterCheckpoint, &afterProxy, &afterBilling)
			if err != nil {
				t.Fatal(err)
			}
			if status != "cancelled" || state != "cancelled" || fee != "released" || code != "admin_closed" || submitted != (mode != "preparing") {
				t.Fatalf("incorrect closure %s %s %s %s %t", status, state, fee, code, submitted)
			}
			if checkpoint != afterCheckpoint || proxy != afterProxy || billing != afterBilling {
				t.Fatal("closure erased original evidence")
			}
			var releases, audits, snapshots int
			h.a.DB.QueryRow(r.ctx, "SELECT count(*) FROM ledger WHERE order_id=$1 AND kind='unfreeze'", r.id).Scan(&releases)
			h.a.DB.QueryRow(r.ctx, "SELECT count(*) FROM audit_events WHERE target=$1 AND action='order.closed'", r.id).Scan(&audits)
			h.a.DB.QueryRow(r.ctx, "SELECT count(*) FROM order_closures WHERE order_id=$1", r.id).Scan(&snapshots)
			if releases != 1 || audits != 1 || snapshots != 1 {
				t.Fatal("duplicate closure", releases, audits, snapshots)
			}
			if err = r.save(nativeCharged(cp, 1500)); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("stale worker accepted", err)
			}
			r.finish(cslive.Result{Checkpoint: nativeCharged(cp, 1500)})
			if err = h.a.finishPayment(r.ctx, r.id, r.attempt, paymentEvent{State: "succeeded", Amount: 1500, Currency: "php"}); err != nil {
				t.Fatal(err)
			}
			if err = h.a.RecoverPayments(r.ctx); err != nil {
				t.Fatal(err)
			}
			if j, e := h.a.Claim(r.ctx); e != nil || j != nil {
				t.Fatal("closed order was requeued")
			}
			expect(t, h.request("POST", "/api/admin/orders/"+r.id+"/payment/action", M{}, h.admin, nil), 409)
			expect(t, h.request("POST", "/api/admin/orders/"+r.id+"/payment/check", M{}, h.admin, nil), 409)
			paymentBalances(t, h, 100000, 0)
		})
	}
}

func TestAdminCloseCannotRefundChargedServiceFee(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	if ok, e := r.approve(1500, "PHP"); !ok || e != nil {
		t.Fatal(e)
	}
	cp = nativeCharged(cp, 1500)
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	expect(t, h.request("POST", "/api/admin/orders/"+r.id+"/close", M{"reason": "fixture close", "accepted": true}, h.admin, nil), 409)
	paymentBalances(t, h, 90000, 0)
	r.finish(cslive.Result{Checkpoint: cp})
}
