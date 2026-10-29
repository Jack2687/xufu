package cslive

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreparedCheckoutRejectsWrongIdentityBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"other_order", "external_url", "wrong_type", "entity", "key", "secret", "return_url"} {
		t.Run(mode, func(t *testing.T) {
			flow, tr, _ := flowFixture(t, true)
			prepared := PreparedCheckout{CheckoutID: "cs_live_Fixture", URL: "https://pay.openai.com/c/pay/cs_live_Fixture#" + strings.Repeat("a", 260), ProcessorEntity: "openai_llc", PublishableKey: "pk_test_Fixture"}
			kind := "hosted"
			switch mode {
			case "other_order":
				prepared.CheckoutID = "cs_live_Other"
			case "external_url":
				prepared.URL = "https://unapproved.example/c/pay/cs_live_Fixture#private"
			case "wrong_type":
				kind = "oaics"
			case "entity":
				prepared.ProcessorEntity = "../invalid"
			case "key":
				prepared.PublishableKey = "invalid"
			case "secret":
				prepared.ClientSecret = "cs_live_Other_secret_wrong"
			case "return_url":
				prepared.ReturnURL = "https://chatgpt.com/checkout/verify?stripe_session_id=cs_live_Other"
			}
			if _, err := flow.importCheckout(prepared, kind); err == nil || len(tr.calls) != 0 {
				t.Fatal("invalid handoff reached network")
			}
		})
	}
}

func TestCheckoutHandoffCredentialsStayPrivate(t *testing.T) {
	result := CheckoutResult{CheckoutID: "cs_live_Fixture", Session: &Session{AccessToken: "PRIVATE_AT", SessionToken: "PRIVATE_SESSION", Cookies: "PRIVATE_COOKIE"}}
	raw, err := json.Marshal(result)
	if err != nil || strings.Contains(string(raw), "PRIVATE") || strings.Contains(result.String(), "PRIVATE") || strings.Contains(result.GoString(), "PRIVATE") {
		t.Fatal("checkout handoff leaked credentials")
	}
}
