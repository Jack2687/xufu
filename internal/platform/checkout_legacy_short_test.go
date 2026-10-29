package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLegacyScriptShortRequests(t *testing.T) {
	for _, product := range []string{"pro", "pro5x", "team", "team_premium"} {
		for _, country := range []string{"PH", "GB"} {
			for _, id := range []string{"cs_live_Test", "oaics_Test"} {
				t.Run(product+country+id, func(t *testing.T) {
					calls, writes := 0, 0
					in := checkoutInput{AccessToken: "PRIVATE_AT", AccountID: "private-account", Product: product, Country: country, Mode: "short", Seats: 2, PremiumSeats: 1, Workspace: "work"}
					c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
						calls++
						if r.URL.Host == "ipinfo.io" {
							return providerResponse(200, M{"country": country, "ip": "203.0.113.2"}), nil
						}
						if r.URL.String() != checkoutEndpoint || r.Method != "POST" {
							t.Fatal("unexpected endpoint")
						}
						if r.Header.Get("User-Agent") != webCheckoutUA || r.Header.Get("Origin") != "https://chatgpt.com" || r.Header.Get("OAI-Language") != "en-US" || r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("OpenAI-Client-Type") != "" {
							t.Fatal("script web headers differ")
						}
						var body M
						if json.NewDecoder(r.Body).Decode(&body) != nil {
							t.Fatal("bad body")
						}
						entry := "all_plans_pricing_modal"
						if strings.HasPrefix(product, "team") {
							entry = "team_workspace_purchase_modal"
						}
						if body["checkout_ui_mode"] != "custom" || body["entry_point"] != entry {
							t.Fatal("script short payload differs")
						}
						return providerResponse(200, M{"checkout_session_id": id, "checkout_ui_mode": "hosted"}), nil // requested mode wins, even if upstream differs
					})}
					out, err := generateCheckout(context.Background(), c, in, func() error { writes++; return nil })
					entity := "openai_llc"
					if country == "GB" {
						entity = "openai_ie"
					}
					if err != nil || out.URL != "https://chatgpt.com/checkout/"+entity+"/"+id || calls != 2 || writes != 1 {
						t.Fatal("short flow did not match script", err)
					}
				})
			}
		}
	}
}

func TestLegacyShortSubscriptionUsesPrivateBrowserSession(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	h.saveTestBilling("subscription@example.com")
	setSubscriptionFlow(t, h, subscriptionFlowLegacy)
	setCheckoutType(t, h, "oaics")
	h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
	started := make(chan paymentInput, 1)
	p := &fakePayment{events: make(chan paymentEvent, 16)}
	h.a.paymentStart = func(_ context.Context, in paymentInput) (paymentProcess, error) { started <- in; return p, nil }
	body := prepareRequest()
	body["workflow"] = "subscription"
	body["card"] = paymentPrepareBody()["card"]
	body["keep_renewal"] = true
	id := str(expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
	h.addProxy("PH", 2)
	j, err := h.a.claim(ctx, "checkout")
	if err != nil || j == nil {
		t.Fatal(err)
	}
	if err = h.a.recordOrderProxyIP(ctx, j, "203.0.113.2"); err != nil {
		t.Fatal(err)
	}
	short := "https://chatgpt.com/checkout/openai_llc/oaics_Test"
	if err = h.a.finishCheckout(ctx, j, checkoutOutput{URL: short}, nil); err != nil {
		t.Fatal(err)
	}
	waitPayment(t, h, id, "preparing")
	select {
	case in := <-started:
		if in.URL != short || in.SessionCookies != "__Secure-next-auth.session-token=TEST_ONLY_SESSION_TOKEN" || !in.AutoConfirm || !validBilling(in.Billing) || in.ExpectedIP != "203.0.113.2" {
			t.Fatal("short browser lost authorization, billing or proxy")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("browser not started")
	}
	p.events <- paymentEvent{Type: "result", State: "failed", Code: "quote_unavailable"}
	waitPayment(t, h, id, "failed")
	assertSubscriptionPrivate(t, h, id)
}
