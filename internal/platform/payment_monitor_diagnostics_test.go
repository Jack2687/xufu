package platform

import (
	"strings"
	"testing"
	"time"
	"xupay/internal/cslive"
)

func TestProtocolCustomCheckoutLinkAndLegacyFallback(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	cp.CheckoutID = "cs_live_CustomFixture"
	cp.Entity = "openai_llc"
	cp.Metadata["checkout_ui_mode"] = "custom"
	delete(cp.Metadata, "hosted_url")
	if _, err := h.a.DB.Exec(r.ctx, "DELETE FROM order_checkout_links WHERE order_id=$1", r.id); err != nil {
		t.Fatal(err)
	}
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	want := "https://chatgpt.com/checkout/openai_llc/cs_live_CustomFixture"
	for _, legacy := range []bool{false, true} {
		if legacy {
			h.a.DB.Exec(r.ctx, "DELETE FROM order_checkout_links WHERE order_id=$1", r.id)
		}
		links, err := h.a.adminCheckoutLinks(r.ctx, r.id)
		if err != nil || len(links) != 1 || links[0]["url"] != want {
			t.Fatalf("legacy=%v links=%v err=%v", legacy, links, err)
		}
		if legacy && links[0]["generated_at"] != nil {
			t.Fatal("invented generation time")
		}
		for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
			res := h.request("GET", path+r.id, nil, h.member, nil)
			if strings.Contains(res.Body.String(), want) || strings.Contains(res.Body.String(), "checkout_links") {
				t.Fatal("customer URL leak")
			}
		}
	}
	cp.Entity = "../../private"
	if protocolCheckoutURL(cp) != "" {
		t.Fatal("unsafe entity")
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestProtocolMonitorDistinguishesNormalAbsentAndAutoHandoff(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	if h.a.paymentDetails(r.ctx, r.id)["automatic_running"] != true {
		t.Fatal("active worker not marked automatic")
	}
	h.a.paymentActions = map[string]*paymentAction{r.id: {state: "verifying", expires: time.Now().Add(time.Minute)}}
	if h.a.paymentDetails(r.ctx, r.id)["automatic_running"] != false {
		t.Fatal("manual fallback hidden during active verification")
	}
	h.a.paymentActions = nil
	for _, code := range []string{"subscription_not_found", "http_404"} {
		r.event(map[string]any{"event": "http_request", "path": "/backend-api/subscriptions", "status": 404, "errorCode": code})
		d := <-h.a.providerDiagnostics
		h.a.saveProviderDiagnostic(d)
	}
	r.event(map[string]any{"event": "payment_diagnostic", "paymentState": "action_required"})
	h.a.saveProviderDiagnostic(<-h.a.providerDiagnostics)
	rows := h.a.paymentTraceDetails(r.ctx, r.id)
	var absent, failed, automatic bool
	for _, row := range rows {
		if row["code"] == "subscription_absent" {
			absent = row["state"] == "completed"
		}
		if row["code"] == "invalid_response" {
			failed = row["state"] == "failed"
		}
		if row["code"] == "verification_continuing" {
			automatic = row["state"] == "running"
		}
	}
	if !absent || !failed || !automatic {
		t.Fatal("incorrect monitoring states")
	}
	secret := "pi_private_secret_value"
	fields := safeDiagnosticDetails(M{"sdk_stage": secret, "sdk_issue": secret, "sdk_resource": secret, "sdk_event": "progress", "sdk_wait_ms": int64(30000)})
	if len(fields) != 2 {
		t.Fatal("unsafe SDK diagnostics", fields)
	}
	r.finish(cslive.Result{Checkpoint: cp})
}
