package platform

import (
	"context"
	"strings"
	"testing"

	"xupay/internal/cslive"
)

func setCheckoutType(t *testing.T, h *harness, kind string) {
	t.Helper()
	cfg := h.a.config(context.Background(), "payment")
	cfg["checkout_type"] = kind
	types := defaultCheckoutTypes()
	for key := range types {
		types[key] = kind
	}
	cfg["checkout_types"] = types
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
}

func TestProtocolShortLinkRequiresOriginalCheckout(t *testing.T) {
	t.Setenv("XUPAY_LOCAL_PROXY", "")
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://chatgpt.com/checkout/openai_llc/oaics_Test", true},
		{"https://chatgpt.com/checkout/openai_llc/oaics_Other", false},
		{"https://chatgpt.com/checkout/other/oaics_Test", false},
		{"https://chatgpt.com/checkout/openai_llc/oaics_Test?redirect=elsewhere", false},
		{"https://chatgpt.com/checkout/openai_llc/oaics_Test#fragment", false},
		{"https://elsewhere.example/checkout/openai_llc/oaics_Test", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			a := &App{checkoutClient: func(_ cslive.Config, hooks cslive.Hooks) protocolCheckoutClient {
				if hooks.Checkpoint != nil {
					t.Fatal("link generation attached payment checkpoint")
				}
				return checkoutClientFunc(func(_ context.Context, _ cslive.Session, o cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
					if o.CheckoutType != "oaics" || o.WithPrice {
						t.Fatal("incorrect short-link request")
					}
					return cslive.CheckoutResult{CheckoutID: "oaics_Test", ProcessorEntity: "openai_llc", CheckoutURL: tc.url}, nil
				})
			}}
			in := checkoutInput{CheckoutType: "oaics", AccessToken: "PRIVATE_AT", SessionToken: "PRIVATE_SESSION", Country: "PH", Product: "pro5x"}
			out, err := a.generateProtocolCheckout(context.Background(), &Job{}, "http://user:pass@proxy.example.com:8080", in)
			if (err == nil) != tc.ok || (tc.ok && out.URL != tc.url) || (!tc.ok && out.URL != "") {
				t.Fatal("incorrect short-link validation", err)
			}
		})
	}
}

func TestCheckoutTypeSettings(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	if configuredCheckoutType(h.a.config(ctx, "payment")) != "hosted" {
		t.Fatal("wrong initial preference")
	}
	setCheckoutType(t, h, "oaics")
	for _, value := range []any{"", "custom", "long", "redirect", nil, true, float64(1)} {
		cfg := h.a.config(ctx, "payment")
		cfg["checkout_type"] = value
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
	}
	cfg := h.a.config(ctx, "payment")
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.member, nil), 403)
	delete(cfg, "checkout_type")
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
	if err := h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	if configuredCheckoutType(h.a.config(ctx, "payment")) != "oaics" {
		t.Fatal("saved preference reset by old client or restart")
	}
	if _, err := h.a.DB.Exec(ctx, "UPDATE settings SET value=value-'checkout_type' WHERE key='payment'"); err != nil {
		t.Fatal(err)
	}
	if err := h.a.seed(ctx); err != nil || configuredCheckoutType(h.a.config(ctx, "payment")) != "hosted" {
		t.Fatal("upgrade did not default to hosted", err)
	}
}

func TestCheckoutTypeAdmissionReplayForBothPages(t *testing.T) {
	for _, workflow := range []string{"checkout", "subscription"} {
		for _, kind := range []string{"hosted", "oaics", "legacy"} {
			for _, route := range []string{"/api/orders", "/api/v1/orders"} {
				t.Run(workflow+"/"+kind+route, func(t *testing.T) {
					h := setupTest(t)
					ctx := context.Background()
					stopFlowTestWorkers(t, h)
					h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
						t.Error("admission unexpectedly started a payment")
						return nil, nil
					}
					wanted := kind
					if kind == "legacy" {
						setSubscriptionFlow(t, h, subscriptionFlowLegacy)
						setCheckoutType(t, h, "oaics")
						wanted = "oaics"
					} else {
						setCheckoutType(t, h, kind)
						wanted = kind
					}
					body := prepareRequest()
					body["workflow"] = workflow
					if workflow == "subscription" {
						h.saveTestBilling("subscription@example.com")
						if _, err := h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID); err != nil {
							t.Fatal(err)
						}
						body["card"] = paymentPrepareBody()["card"]
					}
					headers := map[string]string{"Idempotency-Key": uid()}
					cookie := h.member
					if strings.HasPrefix(route, "/api/v1/") {
						key := expect(t, h.request("POST", "/api/keys", M{"name": "checkout type"}, h.member, nil), 200)["key"].(string)
						headers["Authorization"], cookie = "Bearer "+key, nil
					}
					id := str(expect(t, h.request("POST", route, body, cookie, headers), 200), "id")
					setCheckoutType(t, h, "hosted")
					replay := expect(t, h.request("POST", route, body, cookie, headers), 200)
					var saved string
					if err := h.a.DB.QueryRow(ctx, "SELECT checkout_type FROM checkout_runs WHERE order_id=$1", id).Scan(&saved); err != nil || saved != wanted || replay["id"] != id || replay["replayed"] != true {
						t.Fatal("admitted preference changed", err, saved)
					}
					view := h.request("GET", route+"/"+id, nil, cookie, headers)
					expect(t, view, 200)
					if strings.Contains(view.Body.String(), "checkout_type") {
						t.Fatal("internal preference leaked")
					}
				})
			}
		}
	}
}
