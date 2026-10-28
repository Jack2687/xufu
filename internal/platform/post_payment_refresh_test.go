package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPostPaymentRefreshBeforeCancellation(t *testing.T) {
	for _, mode := range []string{"success", "expired_at", "cancel_already_sent", "refresh_401", "refresh_403", "wrong_account"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h, id := paidRefreshOrder(t, false)
			var enc, billingBefore string
			if err := h.a.DB.QueryRow(ctx, "SELECT s.credentials_enc,b.details_enc FROM subscription_runs s JOIN order_billing b ON b.order_id=s.order_id WHERE s.order_id=$1", id).Scan(&enc, &billingBefore); err != nil {
				t.Fatal(err)
			}
			raw, err := h.a.decrypt(enc)
			if err != nil {
				t.Fatal(err)
			}
			var in checkoutInput
			if err = json.Unmarshal([]byte(raw), &in); err != nil {
				t.Fatal(err)
			}
			in.SessionToken = "TEST_POST_PAYMENT_SESSION"
			in.RefreshedAt, in.RefreshPurpose = time.Now(), "checkout"
			if mode == "expired_at" {
				in.TokenExpiresAt = time.Now().Add(-time.Minute)
			}
			saved, _ := json.Marshal(in)
			enc, _ = h.a.encrypt(string(saved))
			if _, err = h.a.DB.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2,cancel_started=$3 WHERE order_id=$1", id, enc, mode == "cancel_already_sent"); err != nil {
				t.Fatal(err)
			}
			if _, err = h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{activation_check_limit}','1') WHERE key='payment'`); err != nil {
				t.Fatal(err)
			}
			refreshes, reads, cancels := 0, 0, 0
			freshSession := testSession(in.Email)
			freshAT := str(freshSession, "accessToken") + "REFRESHED"
			freshSession["accessToken"] = freshAT
			h.a.renewalClient = func() *http.Client {
				return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/auth/session" {
						refreshes++
						var paid string
						if err := h.a.DB.QueryRow(ctx, "SELECT state FROM payment_runs WHERE order_id=$1", id).Scan(&paid); err != nil || paid != "succeeded" {
							t.Fatal("refresh ran before payment success", err, paid)
						}
						if reads != 0 || cancels != 0 || r.Method != "GET" || r.Header.Get("Authorization") != "" {
							t.Fatal("refresh must precede renewal and use the session cookie")
						}
						cookie, err := r.Cookie("__Secure-next-auth.session-token")
						if err != nil || cookie.Value != in.SessionToken {
							t.Fatal("refresh cookie missing")
						}
						if mode == "refresh_401" || mode == "refresh_403" {
							status := 401
							if mode == "refresh_403" {
								status = 403
							}
							return providerResponse(status, M{"error": "rejected"}), nil
						}
						if mode == "wrong_account" {
							freshSession["account"] = M{"id": "other-account"}
						}
						return providerResponse(200, freshSession), nil
					}
					wantPath := "/backend-api/subscriptions"
					if r.Method == "POST" {
						wantPath += "/cancel"
					}
					if r.URL.Path != wantPath || refreshes != 1 || r.Header.Get("Authorization") != "Bearer "+freshAT {
						t.Fatal("renewal did not use the freshly obtained AT")
					}
					var current string
					if err := h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1", id).Scan(&current); err != nil {
						t.Fatal(err)
					}
					decoded, err := h.a.decrypt(current)
					var persisted checkoutInput
					if err != nil || json.Unmarshal([]byte(decoded), &persisted) != nil || persisted.AccessToken != freshAT {
						t.Fatal("renewal used AT before persisting the refresh")
					}
					if r.Method == "POST" {
						cancels++
						if mode == "cancel_already_sent" || cancels > 1 {
							t.Fatal("cancellation was replayed")
						}
						return providerResponse(200, M{"success": true}), nil
					}
					reads++
					return providerResponse(200, M{"is_active": true, "will_renew": cancels == 0 && mode != "cancel_already_sent", "plan_type": "premium"}), nil
				})}
			}
			next, lease, err := h.a.claimRenewal(ctx)
			if err != nil || next != id {
				t.Fatal("paid renewal not claimed", err)
			}
			h.a.runRenewal(ctx, id, lease)
			var paid, fee, renewal, activation, billingAfter string
			if err = h.a.DB.QueryRow(ctx, "SELECT p.state,p.fee_state,o.renewal,o.activation,b.details_enc FROM payment_runs p JOIN orders o ON o.id=p.order_id JOIN order_billing b ON b.order_id=p.order_id WHERE p.order_id=$1", id).Scan(&paid, &fee, &renewal, &activation, &billingAfter); err != nil {
				t.Fatal(err)
			}
			if paid != "succeeded" || fee != "charged" || billingBefore != billingAfter || refreshes != 1 {
				t.Fatal("paid state, fee, billing or bounded refresh changed", paid, fee, refreshes)
			}
			if mode == "success" || mode == "expired_at" || mode == "cancel_already_sent" {
				wantCancels := 1
				if mode == "cancel_already_sent" {
					wantCancels = 0
				}
				if renewal != "disabled" || activation != "active" || cancels != wantCancels || reads != wantCancels+1 {
					t.Fatal("renewal result not verified", renewal, activation, cancels, reads)
				}
			} else if cancels != 0 || reads != 0 || activation != "review" || renewal != "review" {
				t.Fatal("failed refresh proceeded with stale AT", cancels, reads, activation, renewal)
			}
			paymentBalances(t, h, 90000, 0)
			assertSubscriptionPrivate(t, h, id)
		})
	}
}
