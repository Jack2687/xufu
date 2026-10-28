package platform

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRenewalRejectedAuthorizationStopsWithoutReplaying(t *testing.T) {
	for _, mode := range []string{"subscription", "standalone"} {
		for _, afterWrite := range []bool{false, true} {
			name := mode + "/before_cancel"
			if afterWrite {
				name = mode + "/after_cancel"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var h *harness
				var id string
				if mode == "subscription" {
					var p *fakePayment
					h, id, p = subscriptionFixture(t, false)
					completeSubscriptionCheckout(t, h, id)
					p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
					waitPayment(t, h, id, "submitting")
					p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
					waitPayment(t, h, id, "succeeded")
				} else {
					h = setupTest(t)
					id = newStandalone(t, h, "renewal@example.com")
				}
				writes, reads := 0, 0
				h.a.renewalClient = func() *http.Client {
					return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
						status, body := 401, `{"error":{"code":"token_expired","type":"invalid_request_error","message":"PRIVATE_AUTH_DIAGNOSTIC"}}`
						if r.Method == "POST" {
							writes++
							status, body = 200, `{}`
						} else {
							reads++
							if afterWrite && writes == 0 {
								status, body = 200, `{"is_active":true,"plan_type":"premium","will_renew":true}`
							}
						}
						return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
					})}
				}
				claim, run := h.a.claimRenewal, h.a.runRenewal
				table, idcol, statecol := "subscription_runs", "order_id", "renewal_state"
				if mode == "standalone" {
					claim, run = h.a.claimStandaloneRenewal, h.a.runStandaloneRenewal
					table, idcol, statecol = "renewal_tasks", "id", "status"
				}
				next, lease, err := claim(ctx)
				if err != nil || next != id {
					t.Fatal("claim failed", err)
				}
				run(ctx, id, lease)
				var state, credentials string
				var checks int
				var started, released bool
				err = h.a.DB.QueryRow(ctx, "SELECT "+statecol+",credentials_enc,checks,cancel_started,lease_token IS NULL FROM "+table+" WHERE "+idcol+"=$1", id).Scan(&state, &credentials, &checks, &started, &released)
				if err != nil || state != "review" || credentials != "" || checks != 1 || started != afterWrite || !released {
					t.Fatal("authorization failure not finalized safely", err, state, checks, started, released)
				}
				wantWrites := 0
				if afterWrite {
					wantWrites = 1
				}
				if writes != wantWrites || reads != wantWrites+1 {
					t.Fatal("unexpected provider operations", writes, reads)
				}
				h.a.DB.Exec(ctx, "UPDATE "+table+" SET next_at=now() WHERE "+idcol+"=$1", id)
				if next, _, err := claim(ctx); err != nil || next != "" {
					t.Fatal("rejected credential requeued", err)
				}
				path := "/api/renewals/" + id
				if mode == "subscription" {
					path = "/api/orders/" + id
					var paid, fee string
					h.a.DB.QueryRow(ctx, "SELECT state,fee_state FROM payment_runs WHERE order_id=$1", id).Scan(&paid, &fee)
					if paid != "succeeded" || fee != "charged" {
						t.Fatal("paid state changed", paid, fee)
					}
					assertSubscriptionPrivate(t, h, id)
				}
				response := h.request("GET", path, nil, h.member, nil)
				expect(t, response, 200)
				if !strings.Contains(response.Body.String(), "授权已失效") || strings.Contains(response.Body.String(), "PRIVATE_AUTH_DIAGNOSTIC") || strings.Contains(response.Body.String(), "token_expired") {
					t.Fatal("missing safe reason or raw response leaked")
				}
			})
		}
	}
}
