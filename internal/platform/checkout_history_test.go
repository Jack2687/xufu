package platform

import (
	"context"
	"strings"
	"testing"
)

func TestAdminCheckoutHistorySurvivesPaymentAndCleanup(t *testing.T) {
	h, id, p := subscriptionFixture(t, true)
	ctx := context.Background()
	h.addProxy("PH", 1)
	j, e := h.a.claim(ctx, "checkout")
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if e = h.a.recordOrderProxyIP(ctx, j, "203.0.113.2"); e != nil {
		t.Fatal(e)
	}
	link := "https://pay.openai.com/c/pay/cs_test_history#" + strings.Repeat("long%2Ffragment", 90)
	if e = h.a.finishCheckout(ctx, j, checkoutOutput{URL: link}, nil); e != nil {
		t.Fatal(e)
	}
	// Lease fencing prevents a duplicate completion from replacing history.
	if e = h.a.finishCheckout(ctx, j, checkoutOutput{URL: "https://pay.openai.com/c/pay/cs_test_other"}, nil); e == nil {
		t.Fatal("stale job accepted")
	}
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "succeeded")
	if _, e = h.a.DB.Exec(ctx, "UPDATE checkout_runs SET link_expires_at=now()-interval '1 minute' WHERE order_id=$1", id); e != nil {
		t.Fatal(e)
	}
	if e = h.a.Maintain(ctx); e != nil {
		t.Fatal(e)
	}
	var active, encrypted string
	if e = h.a.DB.QueryRow(ctx, "SELECT c.hosted_url_enc,h.url_enc FROM checkout_runs c JOIN order_checkout_links h ON h.order_id=c.order_id WHERE c.order_id=$1", id).Scan(&active, &encrypted); e != nil {
		t.Fatal(e)
	}
	if active != "" || encrypted == link || strings.Contains(encrypted, "pay.openai.com") {
		t.Fatal("history not independent/encrypted")
	}
	w := h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil)
	v := expect(t, w, 200)
	links := v["checkout_links"].([]any)
	if len(links) != 1 {
		t.Fatal("history missing or duplicated")
	}
	item := links[0].(map[string]any)
	if item["url"] != link || item["generated_at"] == nil || item["round"] != float64(1) {
		t.Fatal("history link, fragment or generation changed")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private history cached")
	}
	for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
		response := h.request("GET", path+id, nil, h.member, nil)
		expect(t, response, 200)
		if strings.Contains(response.Body.String(), "checkout_links") || strings.Contains(response.Body.String(), "cs_test_history") {
			t.Fatal("customer link leak")
		}
	}
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.member, nil), 403)
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, nil, nil), 401)
	// Successful payments remain terminal; looking at history cannot requeue them.
	paymentBalances(t, h, 90000, 0)
}

func TestCheckoutHistoryLegacyAndInvalidLinks(t *testing.T) {
	h, id, _ := paymentFixture(t)
	ctx := context.Background()
	// Upgrade existing ciphertext without inventing a historical creation time.
	for i := 0; i < 2; i++ {
		if _, e := h.a.DB.Exec(ctx, schema); e != nil {
			t.Fatal(e)
		}
	}
	links, e := h.a.adminCheckoutLinks(ctx, id)
	if e != nil || len(links) != 1 {
		t.Fatal("migration missing or duplicated history", e)
	}
	if links[0]["generated_at"] != nil || links[0]["url"] != "https://pay.openai.com/c/pay/cs_test_QA123" {
		t.Fatal("incorrect legacy projection")
	}
	for _, raw := range []string{"javascript:alert(1)", "https://evil.example/c/pay/cs_test_private", "https://user:secret@pay.openai.com/c/pay/cs_test_private"} {
		enc, _ := h.a.encrypt(raw)
		if _, e = h.a.DB.Exec(ctx, "UPDATE order_checkout_links SET url_enc=$2 WHERE order_id=$1", id, enc); e != nil {
			t.Fatal(e)
		}
		links, e = h.a.adminCheckoutLinks(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if links[0]["url"] != nil || links[0]["available"] != false {
			t.Fatal("unsafe link projected")
		}
	}
}
