package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test-only data. No test address is sent to a real payment service.
func (h *harness) saveTestBilling(email string) {
	h.t.Helper()
	address := M{"email": email, "first_name": "QA", "last_name": "Test", "state": "NY", "city": "New York", "line1": "100 Test Street", "postal_code": "10001", "enabled": true, "test_only": false}
	cfg := M{"country": "US", "allow_customer_edit": true, "address_templates": []any{address}}
	expect(h.t, h.request("PUT", "/api/admin/settings/billing", cfg, h.admin, nil), 200)
}

func TestBillingProfileManagement(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	path := "/api/admin/billing-profiles"
	profile := M{"email": "payer@example.com", "first_name": "QA", "last_name": "Test", "state": "NY", "city": "New York", "line1": "100 Test Street", "postal_code": "10001", "enabled": true, "test_only": false}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		endpoint := path
		if method == "PUT" || method == "DELETE" {
			endpoint += "/payer@example.com"
		}
		expect(t, h.request(method, endpoint, profile, nil, nil), 401)
		expect(t, h.request(method, endpoint, profile, h.member, nil), 403)
	}
	expect(t, h.request("POST", path, profile, h.admin, nil), 200)
	profile["email"] = "PAYER@example.com"
	expect(t, h.request("POST", path, profile, h.admin, nil), 400)
	profile["email"] = "payer@example.com"
	if _, e := h.pickBilling(); e != nil {
		t.Fatal(e)
	}
	profile["enabled"] = false
	expect(t, h.request("PUT", path+"/payer@example.com", profile, h.admin, nil), 200)
	if _, e := h.pickBilling(); e == nil {
		t.Fatal("disabled profile used for payment")
	}
	profile["enabled"], profile["test_only"] = true, true
	expect(t, h.request("PUT", path+"/payer@example.com", profile, h.admin, nil), 200)
	if _, e := h.pickBilling(); e == nil {
		t.Fatal("test-only profile used for real payment")
	}
	profile["email"], profile["city"] = "new@example.com", "QA City"
	items := expect(t, h.request("PUT", path+"/payer@example.com", profile, h.admin, nil), 200)["items"].([]any)
	if len(items) != 1 || items[0].(M)["email"] != "new@example.com" || items[0].(M)["city"] != "QA City" {
		t.Fatal("profile edit was not persisted")
	}
	expect(t, h.request("PUT", path+"/payer@example.com", profile, h.admin, nil), 404)
	profile["enabled"] = "true"
	expect(t, h.request("PUT", path+"/new@example.com", profile, h.admin, nil), 400)
	items = expect(t, h.request("DELETE", path+"/new@example.com", nil, h.admin, nil), 200)["items"].([]any)
	if len(items) != 0 {
		t.Fatal("delete did not remove profile")
	}
	expect(t, h.request("DELETE", path+"/new@example.com", nil, h.admin, nil), 404)
	var count int
	if e := h.a.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action LIKE 'billing.%'").Scan(&count); e != nil || count != 5 {
		t.Fatal("billing audit records missing", count, e)
	}
}

func TestBillingConcurrentCreatesPreserveProfiles(t *testing.T) {
	h := setupTest(t)
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			profile := M{"email": fmt.Sprintf("qa%d@example.com", i), "first_name": "QA", "last_name": "Test", "state": "NY", "city": "New York", "line1": "100 Test Street", "postal_code": "10001", "enabled": false, "test_only": true}
			results <- h.request(http.MethodPost, "/api/admin/billing-profiles", profile, h.admin, nil)
		}(i)
	}
	wg.Wait()
	close(results)
	for result := range results {
		expect(t, result, 200)
	}
	items := expect(t, h.request("GET", "/api/admin/billing-profiles", nil, h.admin, nil), 200)["items"].([]any)
	if len(items) != 8 {
		t.Fatal("concurrent profile updates were lost", len(items))
	}
}

func TestBillingTestPoolWithoutEmail(t *testing.T) {
	h := setupTest(t)
	path := "/api/admin/billing-profiles"
	expect(t, h.request("POST", path+"/sample", M{}, nil, nil), 401)
	expect(t, h.request("POST", path+"/sample", M{}, h.member, nil), 403)
	expect(t, h.request("POST", path+"/sample", M{}, h.admin, nil), 400)
	profile := M{"name": "Sample", "first_name": "QA", "last_name": "Test", "state": "NY", "city": "New York", "line1": "100 Test Street", "postal_code": "10001", "enabled": true, "test_only": true}
	items := expect(t, h.request("POST", path, profile, h.admin, nil), 200)["items"].([]any)
	saved := items[0].(M)
	id := str(saved, "id")
	if id == "" || str(saved, "email") != "" {
		t.Fatal("pool profile requires email or has no stable ID")
	}
	for i := 0; i < 3; i++ {
		result := expect(t, h.request("POST", path+"/sample", M{}, h.admin, nil), 200)
		if result["profile"].(M)["id"] != id || result["test_only"] != true {
			t.Fatal("sample selection not limited to enabled test pool")
		}
	}
	saved["enabled"] = false
	expect(t, h.request("PUT", path+"/"+id, saved, h.admin, nil), 200)
	expect(t, h.request("POST", path+"/sample", M{}, h.admin, nil), 400)
	saved["enabled"], saved["test_only"] = true, false
	expect(t, h.request("PUT", path+"/"+id, saved, h.admin, nil), 200)
	expect(t, h.request("POST", path+"/sample", M{}, h.admin, nil), 400)
	expect(t, h.request("DELETE", path+"/"+id, nil, h.admin, nil), 200)
}

func TestSubscriptionDisabledOrTestBillingDoesNotFreeze(t *testing.T) {
	h := setupTest(t)
	h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
		t.Error("ineligible billing started payment runtime")
		return nil, context.Canceled
	}
	ctx := context.Background()
	h.saveTestBilling("subscription@example.com")
	if _, e := h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID); e != nil {
		t.Fatal(e)
	}
	for _, flags := range []M{{"enabled": false, "test_only": false}, {"enabled": true, "test_only": true}} {
		cfg := h.a.config(ctx, "billing")
		profile := cfg["address_templates"].([]any)[0].(M)
		for k, v := range flags {
			profile[k] = v
		}
		expect(t, h.request("PUT", "/api/admin/settings/billing", cfg, h.admin, nil), 200)
		body := prepareRequest()
		body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
		expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 400)
		paymentBalances(t, h, 100000, 0)
	}
	var n int
	if e := h.a.DB.QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n); e != nil || n != 0 {
		t.Fatal("ineligible billing created an order", n, e)
	}
}

func (h *harness) pickBilling() (M, error) {
	ctx := context.Background()
	tx, e := h.a.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	return h.a.randomBillingTx(ctx, tx)
}

func TestBillingLivePoolEligibilityAndEveryCandidate(t *testing.T) {
	first := map[string]any{"id": "bp_a", "first_name": "QA", "last_name": "First", "state": "NY", "city": "New York", "line1": "100 Test Street", "postal_code": "10001", "enabled": true, "test_only": false}
	copyRow := func(id string) map[string]any {
		r := map[string]any{}
		for k, v := range first {
			r[k] = v
		}
		r["id"] = id
		return r
	}
	second := copyRow("bp_b")
	second["email"] = "unrelated@example.com"
	second["state"] = "CA"
	second["city"] = "Los Angeles"
	second["postal_code"] = "90001"
	testOnly := copyRow("bp_test")
	testOnly["test_only"] = true
	disabled := copyRow("bp_disabled")
	disabled["enabled"] = false
	invalid := copyRow("bp_invalid")
	invalid["postal_code"] = "invalid"
	unclassified := copyRow("bp_legacy")
	delete(unclassified, "test_only")
	cfg := M{"address_templates": []any{testOnly, first, disabled, second, invalid, unclassified}}
	for index, want := range []string{"bp_a", "bp_b"} {
		got, e := chooseBillingProfile(cfg, func(n int) (int, error) {
			if n != 2 {
				t.Fatalf("wrong pool size %d", n)
			}
			return index, nil
		})
		if e != nil || got["profile_id"] != want || got["source"] != "saved" {
			t.Fatal("eligible profile not selectable", e, got)
		}
		if _, exists := got["email"]; exists {
			t.Fatal("legacy email copied into payment data")
		}
		got["line1"] = "changed copy"
	}
	if first["line1"] != "100 Test Street" {
		t.Fatal("selection mutated configured profile")
	}
	for _, empty := range []M{{}, {"address_templates": []any{testOnly, disabled, invalid, unclassified}}} {
		if _, e := chooseBillingProfile(empty, func(int) (int, error) { t.Fatal("sampled empty pool"); return 0, nil }); e == nil {
			t.Fatal("empty eligible pool accepted")
		}
	}
	if _, e := chooseBillingProfile(cfg, func(int) (int, error) { return 0, errors.New("entropy failed") }); e == nil {
		t.Fatal("random failure ignored")
	}
}

func TestSubscriptionBillingSnapshotAndReplay(t *testing.T) {
	h, id, p := subscriptionFixture(t, false)
	ctx := context.Background()
	var sealed, key string
	if e := h.a.DB.QueryRow(ctx, "SELECT b.details_enc,o.idempotency_key FROM order_billing b JOIN orders o ON o.id=b.order_id WHERE b.order_id=$1", id).Scan(&sealed, &key); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(sealed, "Test Street") {
		t.Fatal("billing stored without encryption")
	}
	raw, e := h.a.decrypt(sealed)
	if e != nil {
		t.Fatal(e)
	}
	var before M
	if json.Unmarshal([]byte(raw), &before) != nil {
		t.Fatal("invalid snapshot")
	}
	// Removing the configured pool affects new orders, not this admitted order.
	if _, e = h.a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{address_templates}','[]') WHERE key='billing'"); e != nil {
		t.Fatal(e)
	}
	body := prepareRequest()
	body["workflow"], body["card"], body["keep_renewal"] = "subscription", paymentPrepareBody()["card"], false
	replay := expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": key}), 200)
	if replay["id"] != id || replay["replayed"] != true {
		t.Fatal("replay selected new billing")
	}
	tx, e := h.a.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		t.Fatal(e)
	}
	got, e := h.a.orderBillingTx(ctx, tx, id)
	tx.Rollback(ctx)
	if e != nil || got["line1"] != before["line1"] || got["profile_id"] != before["profile_id"] {
		t.Fatal("retry did not preserve billing")
	}
	captured := make(chan M, 1)
	h.a.paymentStart = func(_ context.Context, in paymentInput) (paymentProcess, error) {
		captured <- in.Billing
		return p, nil
	}
	completeSubscriptionCheckout(t, h, id)
	select {
	case actual := <-captured:
		if actual["line1"] != before["line1"] || actual["profile_id"] != before["profile_id"] {
			t.Fatal("runtime changed selected billing")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("payment runtime did not receive original billing")
	}
	p.events <- paymentEvent{Type: "result", State: "failed", Code: "preparation_failed"}
	waitPayment(t, h, id, "failed")
	assertSubscriptionPrivate(t, h, id)
}

func TestSubscriptionMissingBillingDoesNotEnqueueOrFreeze(t *testing.T) {
	h := setupTest(t)
	h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
		t.Error("payment runtime started without saved billing")
		return nil, context.Canceled
	}
	ctx := context.Background()
	if _, e := h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID); e != nil {
		t.Fatal(e)
	}
	body := prepareRequest()
	body["workflow"] = "subscription"
	body["card"] = paymentPrepareBody()["card"]
	expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 400)
	paymentBalances(t, h, 100000, 0)
	for _, table := range []string{"orders", "jobs", "payment_runs", "ledger"} {
		var n int
		if e := h.a.DB.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
			t.Fatal("missing billing changed business data", table, e)
		}
	}
}

func TestPaymentMissingBillingDoesNotStartOrFreeze(t *testing.T) {
	h, id, _ := paymentFixture(t)
	ctx := context.Background()
	if _, e := h.a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{address_templates}','[]') WHERE key='billing'"); e != nil {
		t.Fatal(e)
	}
	expect(t, h.request("POST", "/api/orders/"+id+"/payment/prepare", paymentPrepareBody(), h.member, nil), 400)
	paymentBalances(t, h, 10000, 0)
	var n int
	if e := h.a.DB.QueryRow(ctx, "SELECT count(*) FROM payment_runs").Scan(&n); e != nil || n != 0 {
		t.Fatal("payment started without saved billing", e)
	}
}
