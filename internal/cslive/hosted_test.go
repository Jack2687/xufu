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

func TestHostedConfirmationDiagnosticsSurvivePendingPageReads(t *testing.T) {
	flow, tr, _ := flowFixture(t, false)
	cp := flow.cp
	cp.CheckoutID, cp.PublishableKey = "cs_live_Fixture", "pk_test_Fixture"
	cp.Metadata["checkout_ui_mode"] = "redirect"
	amount := int64(1500)
	cp.Amount = &amount
	flow.stripe.HostedURL = "https://pay.openai.com/c/pay/cs_live_Fixture#" + strings.Repeat("a", 260)
	flow.stripe.Build = map[string]string{"version": "abcdef1234", "rv_ts": "fixture", "rv": "fixture", "sv": "fixture"}
	var diagnostics []model_Event
	flow.observer.sink = func(event model_Event) {
		if event["event"] == "payment_diagnostic" {
			diagnostics = append(diagnostics, event)
		}
	}
	confirms, pages := 0, 0
	tr.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
		switch path {
		case "/edge-internal/card-metadata":
			return response(200, protocol_Map{}), nil
		case "/v1/payment_methods":
			return response(200, protocol_Map{"id": "pm_Fixture"}), nil
		case "/v1/payment_pages/cs_live_Fixture/confirm":
			confirms++
			return response(200, protocol_Map{"status": "open", "payment_status": "unpaid", "submission_attempt": protocol_Map{"state": "requires_approval", "private_value": "PRIVATE_RESPONSE"}}), nil
		case "/v1/payment_pages/cs_live_Fixture":
			pages++
			return response(200, protocol_Map{"status": "open", "state": "active", "payment_status": "unpaid", "payment_intent": nil, "submission_attempt": nil}), nil
		case "/backend-api/payments/checkout/snapshot":
			return response(204, nil), nil
		default:
			t.Fatalf("unexpected request %s %s", method, path)
			return nil, nil
		}
	}
	err := flow.runHostedFlow(model_Card{Number: "4242424242424242", CVC: "123", ExpMonth: "12", ExpYear: "2035"}, model_Billing{Name: "QA", Email: "fixture@example.com", Country: "US", Line1: "100 Test Street", City: "New York", State: "NY", PostalCode: "10001"}, protocol_Map{"amount": 1500, "checksum": "fixture", "eid": "fixture"})
	if err != nil || confirms != 1 || pages != 3 || cp.Payment != "pending" || cp.Phase != "payment_result" {
		t.Fatal("diagnostics changed payment behavior", err, confirms, pages, cp.Payment, cp.Phase)
	}
	var initialApproval, finalPending bool
	for _, event := range diagnostics {
		initialApproval = initialApproval || event["diagnosticSource"] == "submission_attempt" && event["rawState"] == "requires_approval"
		finalPending = finalPending || event["rawState"] == "active" && event["paymentState"] == "pending"
	}
	raw, _ := json.Marshal(diagnostics)
	if !initialApproval || !finalPending || strings.Contains(string(raw), "PRIVATE_RESPONSE") || strings.Contains(string(raw), "4242424242424242") {
		t.Fatal("initial reply lost or unsafe diagnostic emitted")
	}
	flow.providerCode, flow.declineCode = "preserved-provider", "preserved-decline"
	flow.emitPaymentDiagnostics(protocol_Map{"payment_intent": protocol_Map{"status": "requires_payment_method", "last_payment_error": protocol_Map{"code": "card_declined", "decline_code": "generic_decline"}}})
	if cp.Payment != "pending" || flow.providerCode != "preserved-provider" || flow.declineCode != "preserved-decline" {
		t.Fatal("observational diagnostic changed financial result")
	}
}
