package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"xupay/internal/cslive"
)

func setFlowCombination(t *testing.T, h *harness, checkout, payment string) {
	t.Helper()
	cfg := h.a.config(context.Background(), "payment")
	cfg["checkout_flow"], cfg["subscription_flow"] = checkout, payment
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
}

func TestFlowCombinationDefaultsAndSettings(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	// Simulate the installed, unsplit configuration, then a fresh installation.
	for _, old := range []string{"legacy", "cslive", "fresh"} {
		if old == "fresh" {
			_, _ = h.a.DB.Exec(ctx, "DELETE FROM settings WHERE key='payment'")
		} else {
			_, _ = h.a.DB.Exec(ctx, `UPDATE settings SET value=(value-'checkout_flow') || jsonb_build_object('subscription_flow',$1::text) WHERE key='payment'`, old)
		}
		if err := h.a.seed(ctx); err != nil {
			t.Fatal(err)
		}
		cfg := h.a.config(ctx, "payment")
		if configuredCheckoutFlow(cfg) != "cslive" || configuredSubscriptionFlow(cfg) != "legacy" {
			t.Fatal("default must be new checkout + legacy payment", old)
		}
	}
	for _, checkout := range []string{"legacy", "cslive"} {
		for _, payment := range []string{"legacy", "cslive"} {
			setFlowCombination(t, h, checkout, payment)
			if err := h.a.seed(ctx); err != nil {
				t.Fatal(err)
			}
			cfg := h.a.config(ctx, "payment")
			if configuredCheckoutFlow(cfg) != checkout || configuredSubscriptionFlow(cfg) != payment {
				t.Fatal("saved combination reset")
			}
			delete(cfg, "checkout_flow")
			delete(cfg, "subscription_flow")
			expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
			cfg = h.a.config(ctx, "payment")
			if configuredCheckoutFlow(cfg) != checkout || configuredSubscriptionFlow(cfg) != payment {
				t.Fatal("omitted settings reset")
			}
		}
	}
	for _, invalid := range []any{"", "auto", nil, true} {
		cfg := h.a.config(ctx, "payment")
		cfg["checkout_flow"] = invalid
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
	}
}

func TestFlowCombinationAdmissionAndLinkIsolation(t *testing.T) {
	for _, checkout := range []string{"legacy", "cslive"} {
		for _, payment := range []string{"legacy", "cslive"} {
			t.Run(checkout+"_"+payment, func(t *testing.T) {
				h := setupTest(t)
				stopFlowTestWorkers(t, h)
				ctx := context.Background()
				h.saveTestBilling("subscription@example.com")
				h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
					t.Error("unexpected payment")
					return nil, nil
				}
				_, _ = h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
				setFlowCombination(t, h, checkout, payment)
				body := prepareRequest()
				body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
				id := str(expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
				var actual string
				if err := h.a.DB.QueryRow(ctx, "SELECT flow FROM checkout_runs WHERE order_id=$1", id).Scan(&actual); err != nil || actual != checkout {
					t.Fatal("wrong checkout stage", err)
				}
				if h.a.isProtocolSubscription(ctx, id) != (payment == "cslive") {
					t.Fatal("wrong payment stage")
				}
				order := expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil), 200)["order"].(M)
				if order["checkout_flow"] != checkout || order["subscription_flow"] != payment {
					t.Fatal("monitor lost combination")
				}
				customer := h.request("GET", "/api/orders/"+id, nil, h.member, nil)
				if strings.Contains(customer.Body.String(), "checkout_flow") || strings.Contains(customer.Body.String(), "subscription_flow") {
					t.Fatal("private flow exposed")
				}
				// Independent link generation depends only on its own stage.
				link := prepareRequest()
				delete(link["session"].(M), "sessionToken")
				want := 400
				if checkout == "legacy" {
					want = 200
				}
				expect(t, h.request("POST", "/api/session/inspect", M{"session": link["session"], "workflow": "checkout"}, h.member, nil), want)
				out := expect(t, h.request("POST", "/api/orders", link, h.member, map[string]string{"Idempotency-Key": uid()}), want)
				if want == 200 && h.a.isProtocolSubscription(ctx, str(out, "id")) {
					t.Fatal("link tool gained payment stage")
				}
			})
		}
	}
}

// Real admission, claim, checkout dispatch, encrypted handoff and settlement;
// only external transports/browser processes are replaced with offline fixtures.
func TestMixedSubscriptionWorkers(t *testing.T) {
	for _, payment := range []string{"legacy", "cslive"} {
		for _, kind := range []string{"hosted", "oaics"} {
			for _, outcome := range []string{"cancel", "keep", "drift"} {
				t.Run(payment+"_"+kind+"_"+outcome, func(t *testing.T) {
					h := setupTest(t)
					stopFlowTestWorkers(t, h)
					t.Setenv("XUPAY_LOCAL_PROXY", "")
					ctx := context.Background()
					h.saveTestBilling("subscription@example.com")
					h.addProxy("PH", 2)
					_, _ = h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
					checkout := "legacy"
					if payment == "legacy" {
						checkout = "cslive"
					}
					setFlowCombination(t, h, checkout, payment)
					setCheckoutType(t, h, kind)
					link := "https://pay.openai.com/c/pay/cs_live_Mixed#" + strings.Repeat("a", 260)
					if kind == "oaics" {
						link = "https://chatgpt.com/checkout/openai_llc/cs_live_Mixed"
					}
					process := &fakePayment{events: make(chan paymentEvent, 8)}
					inputs := make(chan paymentInput, 1)
					h.a.paymentStart = func(_ context.Context, in paymentInput) (paymentProcess, error) { inputs <- in; return process, nil }
					body := prepareRequest()
					body["workflow"], body["card"], body["keep_renewal"] = "subscription", paymentPrepareBody()["card"], outcome == "keep"
					id := str(expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
					job, err := h.a.Claim(ctx)
					if err != nil || job == nil {
						t.Fatal("claim", err)
					}
					var originalProxy, originalBilling string
					if err = h.a.DB.QueryRow(ctx, "SELECT endpoint_enc,details_enc FROM order_proxy_sessions JOIN order_billing USING(order_id) WHERE order_id=$1", id).Scan(&originalProxy, &originalBilling); err != nil {
						t.Fatal(err)
					}
					endpoint, _ := h.a.decrypt(originalProxy)
					creates, pays, cancels, refreshes := 0, 0, 0, 0
					refreshed := cslive.Session{AccessToken: "PRIVATE_REFRESHED_AT", SessionToken: "PRIVATE_REFRESHED_SESSION", Cookies: "__Secure-next-auth.session-token=PRIVATE_REFRESHED_COOKIE", AccountID: "test-space", Email: "subscription@example.com"}
					h.a.checkoutClient = func(_ cslive.Config, _ cslive.Hooks) protocolCheckoutClient {
						if checkout != "cslive" {
							t.Fatal("wrong checkout generator")
						}
						return checkoutClientFunc(func(_ context.Context, _ cslive.Session, o cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
							creates++
							if o.CheckoutType != kind {
								t.Fatal("changed checkout type")
							}
							if err := h.a.recordOrderProxyIP(ctx, job, "203.0.113.2"); err != nil {
								t.Fatal(err)
							}
							if err := h.a.BeforeSideEffect(ctx, job); err != nil {
								t.Fatal(err)
							}
							return cslive.CheckoutResult{CheckoutID: "cs_live_Mixed", CheckoutURL: link, ProcessorEntity: "openai_llc", Session: &refreshed}, nil
						})
					}
					h.a.legacyCheckoutClient = func(_ context.Context, selected string) (*http.Client, func(), error) {
						if checkout != "legacy" || selected != endpoint {
							t.Fatal("wrong generator or proxy")
						}
						return &http.Client{Transport: testRoundTrip(func(req *http.Request) (*http.Response, error) {
							if req.URL.Hostname() == "ipinfo.io" {
								return providerResponse(200, M{"country": "PH", "ip": "203.0.113.2"}), nil
							}
							if req.URL.String() != checkoutEndpoint {
								t.Fatal("legacy repeated preflight")
							}
							creates++
							return providerResponse(200, M{"checkout_session_id": "cs_live_Mixed", "processor_entity": "openai_llc", "publishable_key": "pk_test_Mixed", "checkout_ui_mode": "hosted", "tag": "hosted_checkout_session", "url": link}), nil
						})}, func() {}, nil
					}
					h.a.protocolClient = func(r *protocolRun, _ int) (protocolPaymentClient, func(), error) {
						if payment != "cslive" {
							t.Fatal("wrong payment executor")
						}
						return fixtureProtocolClient{pay: func(_ context.Context, s cslive.Session, _ cslive.Card, o cslive.PayOptions) (cslive.Result, error) {
							pays++
							if o.PreparedCheckout == nil || o.PreparedCheckout.URL != link || o.PreparedCheckout.CheckoutID != "cs_live_Mixed" || o.CheckoutType != kind || o.CancelRenewal == nil || *o.CancelRenewal != (outcome != "keep") {
								t.Fatal("lost original checkout/options")
							}
							if err := r.guardProxy(ctx); err != nil {
								return cslive.Result{}, err
							}
							var billing M
							raw, _ := h.a.decrypt(originalBilling)
							_ = json.Unmarshal([]byte(raw), &billing)
							if o.Billing.Line1 != str(billing, "line1") || o.Billing.PostalCode != str(billing, "postal_code") {
								t.Fatal("billing changed")
							}
							cp := cslive.DefaultCheckpoint()
							cp.AccountID, cp.Email, cp.AccessToken, cp.SessionToken = s.AccountID, s.Email, s.AccessToken, s.SessionToken
							cp.CheckoutID, cp.PaymentIntentID, cp.Currency = "cs_live_Mixed", "pi_Mixed", "PHP"
							if ok, err := o.ApprovePayment(1500, "PHP"); !ok || err != nil {
								t.Fatal("approve", err)
							}
							cp = nativeCharged(cp, 1500)
							cp.Activation, cp.Renewal, cp.Phase = "active", "off", "completed"
							return cslive.Result{Checkpoint: cp}, nil
						}}, func() {}, nil
					}
					// Changing the live configuration cannot change this admitted pair.
					setFlowCombination(t, h, payment, checkout)
					h.a.executeCheckout(ctx, job)
					if creates != 1 {
						t.Fatal("checkout must be created once", creates)
					}
					if payment == "legacy" {
						select {
						case in := <-inputs:
							if in.URL != link || !in.SkipExitIPCheck || !in.AutoConfirm || in.Proxy.Server == "" {
								t.Fatal("browser lost original context")
							}
							if kind == "oaics" && in.SessionCookies != refreshed.Cookies {
								t.Fatal("browser used pre-refresh cookies")
							}
						case <-time.After(5 * time.Second):
							t.Fatal("browser not started")
						}
						process.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
						waitPayment(t, h, id, "submitting")
						process.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
						waitPayment(t, h, id, "succeeded")
						h.a.renewalProxyClient = func(selected string, _ time.Duration) (*http.Client, func(), error) {
							if selected != endpoint {
								t.Fatal("post-payment changed proxy")
							}
							return &http.Client{Transport: testRoundTrip(func(req *http.Request) (*http.Response, error) {
								if req.URL.Hostname() == "ipinfo.io" {
									t.Fatal("subscription performed an exit-IP probe")
									ip := "203.0.113.2"
									if outcome == "drift" {
										ip = "203.0.113.3"
									}
									return providerResponse(200, M{"ip": ip, "country": "PH"}), nil
								}
								if req.URL.Path == "/api/auth/session" {
									refreshes++
									return providerResponse(200, testSession("subscription@example.com")), nil
								}
								if refreshes == 0 && req.Header.Get("Authorization") != "Bearer "+refreshed.AccessToken {
									t.Fatal("activation used old access token")
								}
								if req.Method == "POST" {
									cancels++
									return providerResponse(200, M{}), nil
								}
								return providerResponse(200, M{"id": "mixed-subscription", "is_active": true, "plan_type": "premium", "will_renew": cancels == 0}), nil
							})}, func() {}, nil
						}
						next, lease, err := h.a.claimRenewal(ctx)
						if err != nil || next != id {
							t.Fatal("post-payment task", err)
						}
						h.a.runRenewal(ctx, id, lease)
						if outcome != "keep" && (cancels != 1 || refreshes != 1) {
							t.Fatal("legacy renewal not completed", cancels, refreshes)
						}
						if outcome == "keep" && (cancels != 0 || refreshes != 0) {
							t.Fatal("unexpected renewal mutation")
						}
					} else {
						if pays != 1 {
							t.Fatal("wrong native payment count")
						}
						waitPayment(t, h, id, "succeeded")
					}
					var afterProxy, afterBilling, renewal, activation string
					if err = h.a.DB.QueryRow(ctx, "SELECT endpoint_enc,details_enc,renewal,activation FROM order_proxy_sessions JOIN order_billing USING(order_id) JOIN orders o ON o.id=order_id WHERE order_id=$1", id).Scan(&afterProxy, &afterBilling, &renewal, &activation); err != nil {
						t.Fatal(err)
					}
					if afterProxy != originalProxy || afterBilling != originalBilling {
						t.Fatal("fixed proxy or billing changed")
					}
					{
						want := "disabled"
						if outcome == "keep" {
							want = "kept"
						}
						if activation != "active" || renewal != want {
							t.Fatal("lifecycle incomplete", activation, renewal)
						}
						paymentBalances(t, h, 90000, 0)
					}
					assertSubscriptionPrivate(t, h, id)
				})
			}
		}
	}
}
