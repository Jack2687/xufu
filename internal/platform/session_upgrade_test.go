package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestActivationTimeoutAndRecoveryMustMatchPlan(t *testing.T) {
	ctx := context.Background()
	h, id := paidRefreshOrder(t, false)
	h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{activation_check_limit}','2') WHERE key='payment'`)
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				t.Fatal("inactive subscription caused cancellation")
			}
			return providerResponse(404, M{"detail": M{"code": "subscription_not_found"}}), nil
		})}
	}
	for i := 0; i < 2; i++ {
		h.a.DB.Exec(ctx, "UPDATE subscription_runs SET next_at=now() WHERE order_id=$1", id)
		next, lease, e := h.a.claimRenewal(ctx)
		if e != nil || next != id {
			t.Fatal(e)
		}
		h.a.runRenewal(ctx, id, lease)
	}
	var activation, status, paid, fee string
	h.a.DB.QueryRow(ctx, `SELECT o.activation,o.status,p.state,p.fee_state FROM orders o JOIN payment_runs p ON p.order_id=o.id WHERE o.id=$1`, id).Scan(&activation, &status, &paid, &fee)
	if activation != "review" || status != "succeeded" || paid != "succeeded" || fee != "charged" {
		t.Fatal(activation, status, paid, fee)
	}
	h.resetRates()
	task := newStandalone(t, h, "subscription@example.com")
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				t.Fatal("different plan was cancelled")
			}
			return providerResponse(200, M{"is_active": true, "will_renew": false, "plan_type": "standard"}), nil
		})}
	}
	runStandalone(t, h, task)
	standaloneStatus(t, h, task, "review")
	h.a.DB.QueryRow(ctx, "SELECT activation FROM orders WHERE id=$1", id).Scan(&activation)
	if activation != "review" {
		t.Fatal("wrong plan resolved activation")
	}
	paymentBalances(t, h, 90000, 0)
}

func TestRefreshCredentialsLeaseAndCleanup(t *testing.T) {
	ctx := context.Background()
	h, id := paidRefreshOrder(t, false)
	next, lease, e := h.a.claimRenewal(ctx)
	if e != nil || next != id {
		t.Fatal(e)
	}
	in, _ := refreshFixture(t)
	if e = h.a.saveRenewalCredentials(ctx, id, "stale", in, false); !errors.Is(e, ErrLeaseLost) {
		t.Fatal("stale worker saved auth", e)
	}
	if e = h.a.saveRenewalCredentials(ctx, id, lease, in, false); e != nil {
		t.Fatal(e)
	}
	h.a.DB.Exec(ctx, "UPDATE subscription_runs SET lease_until=now()-interval '1 second' WHERE order_id=$1", id)
	if e = h.a.saveRenewalCredentials(ctx, id, lease, in, false); !errors.Is(e, ErrLeaseLost) {
		t.Fatal("expired lease saved auth", e)
	}
	var enc string
	h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1", id).Scan(&enc)
	if strings.Contains(enc, "TEST_PRIVATE_SESSION") {
		t.Fatal("unencrypted session")
	}
	raw, _ := h.a.decrypt(enc)
	var saved checkoutInput
	json.Unmarshal([]byte(raw), &saved)
	if saved.SessionToken != in.SessionToken {
		t.Fatal("lost refresh credentials")
	}
	h.a.DB.Exec(ctx, "UPDATE subscription_runs SET lease_until=now()+interval '1 minute',credentials_expires_at=now()-interval '1 second' WHERE order_id=$1", id)
	h.a.runRenewal(ctx, id, lease)
	h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1", id).Scan(&enc)
	if enc != "" {
		t.Fatal("expired auth retained")
	}
}

func TestPaymentEvidenceBindingAndPrivatePersistence(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "matched", false: "wrong_checkout"}[valid], func(t *testing.T) {
			h, id, p := subscriptionFixture(t, false)
			completeSubscriptionCheckout(t, h, id)
			p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
			waitPayment(t, h, id, "submitting")
			received := int64(1500)
			checkout := "cs_test_SubscriptionPrivate"
			if !valid {
				checkout = "cs_test_other"
			}
			p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php", Evidence: &paymentEvidence{CheckoutID: checkout, IntentID: "pi_QATest123", Status: "succeeded", AmountReceived: &received, Currency: "php"}}
			want := "review"
			if valid {
				want = "succeeded"
			}
			waitPayment(t, h, id, want)
			var enc string
			var amount *int64
			h.a.DB.QueryRow(context.Background(), "SELECT payment_evidence_enc,charged_amount FROM payment_runs WHERE order_id=$1", id).Scan(&enc, &amount)
			if valid && (enc == "" || strings.Contains(enc, "pi_QATest123") || amount == nil || *amount != 1500) {
				t.Fatal("missing encrypted evidence")
			}
			if !valid && (enc != "" || amount != nil) {
				t.Fatal("unbound evidence saved")
			}
			for _, path := range []string{"/api/orders/" + id, "/api/admin/orders/" + id} {
				cookie := h.member
				if strings.Contains(path, "admin") {
					cookie = h.admin
				}
				r := h.request("GET", path, nil, cookie, nil)
				expect(t, r, 200)
				if strings.Contains(r.Body.String(), "pi_QATest123") || strings.Contains(r.Body.String(), "payment_evidence_enc") {
					t.Fatal("private evidence exposed")
				}
			}
		})
	}
}

func TestRefreshPersistsCheckoutAuthorizationOnlyWithLiveLease(t *testing.T) {
	h, id, _ := subscriptionFixture(t, false)
	h.addProxy("PH", 2)
	ctx := context.Background()
	job, e := h.a.claim(ctx, "checkout")
	if e != nil || job == nil || job.OrderID != id {
		t.Fatal(e)
	}
	in, _ := refreshFixture(t)
	if e = h.a.saveCheckoutCredentials(ctx, job, in); e != nil {
		t.Fatal(e)
	}
	var checkout, renewal string
	h.a.DB.QueryRow(ctx, "SELECT c.credentials_enc,s.credentials_enc FROM checkout_runs c JOIN subscription_runs s ON s.order_id=c.order_id WHERE c.order_id=$1", id).Scan(&checkout, &renewal)
	if checkout != renewal || checkout == "" {
		t.Fatal("authorization not shared atomically")
	}
	h.a.DB.Exec(ctx, "UPDATE jobs SET lease_token='replacement' WHERE id=$1", job.ID)
	if e = h.a.saveCheckoutCredentials(ctx, job, in); !errors.Is(e, ErrLeaseLost) {
		t.Fatal("stale checkout wrote authorization", e)
	}
}
