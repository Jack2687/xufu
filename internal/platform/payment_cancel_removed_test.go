package platform

import (
	"context"
	"strings"
	"testing"
)

func TestPaymentManualCancellationRemoved(t *testing.T) {
	for _, workflow := range []string{"subscription", "checkout"} {
		t.Run(workflow, func(t *testing.T) {
			ctx := context.Background()
			var h *harness
			var id string
			var p *fakePayment
			if workflow == "subscription" {
				h, id, p = subscriptionFixture(t, false)
			} else {
				h, id, p = paymentFixture(t)
				expect(t, h.request("POST", "/api/orders/"+id+"/payment/prepare", paymentPrepareBody(), h.member, nil), 202)
			}
			phases := []string{"preparing"}
			if workflow == "checkout" {
				phases = append(phases, "quoted")
			}
			for _, phase := range phases {
				if phase == "quoted" {
					p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
					waitPayment(t, h, id, "quoted")
				}
				for _, prefix := range []string{"/api/orders/", "/api/v1/orders/"} {
					expect(t, h.request("POST", prefix+id+"/payment/cancel", M{}, h.member, nil), 404)
					expect(t, h.request("POST", prefix+id+"/cancel", M{}, h.member, nil), 409)
					expect(t, h.request("POST", prefix+id+"/cancel", M{}, h.other, nil), 404)
				}
				var state, fee string
				var released int
				if err := h.a.DB.QueryRow(ctx, "SELECT state,fee_state FROM payment_runs WHERE order_id=$1", id).Scan(&state, &fee); err != nil || state != phase || fee != "held" {
					t.Fatal("cancel request changed payment", state, fee, err)
				}
				h.a.DB.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE order_id=$1 AND kind='unfreeze'", id).Scan(&released)
				if released != 0 {
					t.Fatal("manual cancellation released funds")
				}
			}
			// Automatic timeout remains responsible for releasing funds when no charge was submitted.
			h.a.DB.Exec(ctx, "UPDATE payment_runs SET expires_at=now()-interval '1 second' WHERE order_id=$1", id)
			if err := h.a.RecoverPayments(ctx); err != nil {
				t.Fatal(err)
			}
			waitPayment(t, h, id, "failed")
			if workflow == "subscription" {
				paymentBalances(t, h, 100000, 0)
			} else {
				paymentBalances(t, h, 10000, 0)
			}
			if p.commits.Load() != 0 {
				t.Fatal("timeout submitted payment")
			}
			response := h.request("GET", "/api/openapi.json", nil, nil, nil)
			expect(t, response, 200)
			if strings.Contains(response.Body.String(), "/payment/cancel") {
				t.Fatal("removed feature remains advertised")
			}
		})
	}
}
