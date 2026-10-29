package platform

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestHostedApprovalFenceAndOriginalQuote(t *testing.T) {
	h, id, p := subscriptionFixture(t, false)
	ctx := context.Background()
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	var attempt string
	if err := h.a.DB.QueryRow(ctx, "SELECT attempt_id FROM payment_runs WHERE order_id=$1", id).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	event := paymentEvent{Type: "approval_requested", Amount: 1500, Currency: "php", Evidence: &paymentEvidence{CheckoutID: "cs_test_SubscriptionPrivate", Status: "pending", Currency: "php"}}
	wrong := event
	wrong.Amount = 1600
	if h.a.authorizeHostedApproval(ctx, id, attempt, wrong) == nil {
		t.Fatal("changed price approved")
	}
	wrong = event
	wrong.Evidence = &paymentEvidence{CheckoutID: "cs_test_Other", Status: "pending", Currency: "php"}
	if h.a.authorizeHostedApproval(ctx, id, attempt, wrong) == nil {
		t.Fatal("foreign checkout approved")
	}
	if h.a.authorizeHostedApproval(ctx, id, "stale", event) == nil {
		t.Fatal("stale attempt approved")
	}
	var approved atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.a.authorizeHostedApproval(ctx, id, attempt, event) == nil {
				approved.Add(1)
			}
		}()
	}
	wg.Wait()
	if approved.Load() != 1 {
		t.Fatal("approval was not fenced", approved.Load())
	}
	p.events <- paymentEvent{Type: "result", State: "unknown", Sent: true, Code: "result_unconfirmed"}
	waitPayment(t, h, id, "review")
	if err := h.a.RecoverPayments(ctx); err != nil {
		t.Fatal(err)
	}
	var fenced bool
	if err := h.a.DB.QueryRow(ctx, "SELECT approval_started FROM payment_runs WHERE order_id=$1", id).Scan(&fenced); err != nil || !fenced {
		t.Fatal("approval fence lost", err)
	}
	if h.a.authorizeHostedApproval(ctx, id, attempt, event) == nil {
		t.Fatal("approval repeated after recovery")
	}
	paymentBalances(t, h, 90000, 10000)
}

func TestHostedApprovalResumeRequiresRecordedPendingAndAdministrator(t *testing.T) {
	h, id, p := subscriptionFixture(t, false)
	ctx := context.Background()
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "unknown", Sent: true, Code: "result_unconfirmed"}
	waitPayment(t, h, id, "review")
	endpoint := "/api/admin/orders/" + id + "/payment/check?continue_approval=true"
	expect(t, h.request("POST", endpoint, M{}, h.member, nil), 403)
	expect(t, h.request("POST", endpoint, M{}, h.admin, nil), 409)
	if _, err := h.a.DB.Exec(ctx, `INSERT INTO payment_trace_events(order_id,phase,state,code,round,diagnostics) VALUES($1,'checking','completed','',1,'{"diagnostic_source":"submission_attempt","raw_state":"requires_approval"}')`, id); err != nil {
		t.Fatal(err)
	}
	// Invalid/expired login prevents approval in the worker without clearing the fence.
	expect(t, h.request("POST", endpoint, M{}, h.admin, nil), 202)
	waitPayment(t, h, id, "review")
	var fenced bool
	if err := h.a.DB.QueryRow(ctx, "SELECT approval_started FROM payment_runs WHERE order_id=$1", id).Scan(&fenced); err != nil || fenced {
		t.Fatal("missing login started approval", err)
	}
}
