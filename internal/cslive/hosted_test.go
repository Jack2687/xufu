package cslive

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestHostedPaymentStoresEvidenceBeforeAuxiliaryFailure(t *testing.T) {
	flow, tr, saved := flowFixture(t, true)
	cp := flow.cp
	cp.CheckoutID, cp.PublishableKey = "cs_live_Fixture", "pk_test_Fixture"
	cp.Metadata["checkout_ui_mode"] = "redirect"
	amount := int64(1500)
	cp.Amount = &amount
	flow.stripe.HostedURL = "https://pay.openai.com/c/pay/cs_live_Fixture#" + strings.Repeat("a", 260)
	flow.stripe.Build = map[string]string{"version": "abcdef1234", "rv_ts": "fixture", "rv": "fixture", "sv": "fixture"}
	methods, confirms := 0, 0
	tr.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
		switch path {
		case "/edge-internal/card-metadata":
			return response(200, protocol_Map{}), nil
		case "/v1/payment_methods":
			methods++
			return response(200, protocol_Map{"id": "pm_Fixture"}), nil
		case "/v1/payment_pages/cs_live_Fixture/confirm":
			confirms++
			if len(*saved) == 0 || (*saved)[len(*saved)-1].Payment != "unknown" {
				t.Fatal("missing pre-submit checkpoint")
			}
			raw, _ := json.Marshal((*saved)[len(*saved)-1])
			if strings.Contains(string(raw), "4242424242424242") || strings.Contains(string(raw), `"cvc"`) {
				t.Fatal("card entered checkpoint")
			}
			return response(200, protocol_Map{"status": "complete", "payment_status": "paid", "payment_intent": protocol_Map{"id": "pi_Fixture", "client_secret": "pi_Fixture_secret_Fixture", "status": "succeeded", "currency": "php", "amount_received": 1500}}), nil
		case "/backend-api/payments/checkout/snapshot":
			if (*saved)[len(*saved)-1].Payment != "charged" {
				t.Fatal("auxiliary request preceded durable charge evidence")
			}
			return nil, errors.New("simulated auxiliary network failure")
		default:
			t.Fatalf("unexpected %s %s", method, path)
			return nil, nil
		}
	}
	err := flow.runHostedFlow(model_Card{Number: "4242424242424242", CVC: "123", ExpMonth: "12", ExpYear: "2035"}, model_Billing{Name: "QA", Email: "fixture@example.com", Country: "US", Line1: "100 Test Street", City: "New York", State: "NY", PostalCode: "10001"}, protocol_Map{"amount": 1500, "checksum": "fixture", "eid": "fixture"})
	if err == nil || cp.Payment != "charged" || cp.ChargedPrice() == nil || cp.ChargedPrice().AmountMinor != 1500 || methods != 1 || confirms != 1 {
		t.Fatal("hosted flow lost authoritative charge", err, cp.Payment, methods, confirms)
	}
}
