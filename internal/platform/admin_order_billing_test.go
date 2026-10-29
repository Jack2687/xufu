package platform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAdminOrderBillingSnapshotPrivacyAndReadOnly(t *testing.T) {
	h, id, _ := subscriptionFixture(t, false)
	ctx := context.Background()
	var sealed string
	if err := h.a.DB.QueryRow(ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	raw, err := h.a.decrypt(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var saved M
	if err = json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	saved["unexpected_secret"] = "PRIVATE_FIELD_MUST_NOT_LEAK"
	rawBytes, _ := json.Marshal(saved)
	sealed, err = h.a.encrypt(string(rawBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.a.DB.Exec(ctx, "UPDATE order_billing SET details_enc=$2 WHERE order_id=$1", id, sealed); err != nil {
		t.Fatal(err)
	}
	// Deleting the configured pool must not change the historical order view.
	if _, err = h.a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{address_templates}','[]') WHERE key='billing'"); err != nil {
		t.Fatal(err)
	}
	response := h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil)
	detail := expect(t, response, 200)
	billing := detail["billing"].(M)
	if billing["state"] != "selected" || billing["line1"] != saved["line1"] || billing["profile_id"] != saved["profile_id"] || billing["region"] != saved["state"] {
		t.Fatal("saved billing not displayed")
	}
	if strings.Contains(response.Body.String(), "PRIVATE_FIELD_MUST_NOT_LEAK") || strings.Contains(response.Body.String(), sealed) {
		t.Fatal("non-whitelisted billing disclosed")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("billing view cached")
	}
	for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
		response := h.request("GET", path+id, nil, h.member, nil)
		expect(t, response, 200)
		for _, secret := range []string{"\"billing\"", "profile_id", str(saved, "line1"), "PRIVATE_FIELD_MUST_NOT_LEAK"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatal("customer billing disclosure", path)
			}
		}
	}
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.member, nil), 403)
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, nil, nil), 401)
	var after string
	h.a.DB.QueryRow(ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", id).Scan(&after)
	if after != sealed {
		t.Fatal("view changed billing snapshot")
	}
	if _, err = h.a.DB.Exec(ctx, "UPDATE order_billing SET details_enc='damaged' WHERE order_id=$1", id); err != nil {
		t.Fatal(err)
	}
	detail = expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil), 200)
	if detail["billing"].(M)["state"] != "unavailable" {
		t.Fatal("unreadable snapshot not identified")
	}
	if _, err = h.a.DB.Exec(ctx, "DELETE FROM order_billing WHERE order_id=$1", id); err != nil {
		t.Fatal(err)
	}
	detail = expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil), 200)
	if detail["billing"].(M)["state"] != "not_selected" {
		t.Fatal("missing snapshot not identified")
	}
	var count int
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM order_billing WHERE order_id=$1", id).Scan(&count)
	if count != 0 {
		t.Fatal("view selected new billing")
	}
}
