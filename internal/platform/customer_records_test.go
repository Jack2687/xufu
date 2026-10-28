package platform

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCustomerRecordProjections(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	id := h.draft(uid())["id"].(string)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.a.DB.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE orders SET workflow='subscription',fee=10000,price_snapshot='{"standard":12500,"fee":10000,"tier_name":"测试会员","discount":80,"private_marker":"PRIVATE_RECORD_SECRET"}' WHERE id=$1`, id)
	exec(`UPDATE users SET balance=100000 WHERE id=$1`, h.memberID)
	exec(`INSERT INTO payment_runs(order_id,attempt_id,state,amount,currency,charged_amount,charged_currency,last4,expires_at,hosted_url_enc,error_code)
 VALUES($1,$2,'failed',29999,'jpy',29999,'JPY','4242',now()+interval '1 hour','PRIVATE_RECORD_SECRET','PRIVATE_RECORD_SECRET')`, id, uid())
	funds := func(action string) {
		t.Helper()
		tx, e := h.a.DB.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = paymentFunds(ctx, tx, id, "record-attempt", action); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	funds("hold") // First ledger row, despite a pre-existing balance.
	check := func(available, frozen, balance, held float64) {
		t.Helper()
		v := expect(t, h.request("GET", "/api/wallet", nil, h.member, nil), 200)
		row := v["ledger"].([]any)[0].(map[string]any)
		if row["available_delta"] != available || row["frozen_delta"] != frozen || row["balance_after"] != balance || row["frozen_after"] != held {
			t.Fatalf("incorrect fund movement: %#v", row)
		}
	}
	check(-10000, 10000, 90000, 10000)
	funds("charge")
	check(0, -10000, 90000, 0)
	// A second independent hold/release must show both sides of the transfer.
	tx, e := h.a.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"hold", "release"} {
		if e = paymentFunds(ctx, tx, id, "second-record-attempt", action); e != nil {
			tx.Rollback(ctx)
			t.Fatal(e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	check(10000, -10000, 90000, 0)

	for _, path := range []string{"/api/orders", "/api/wallet"} {
		w := h.request("GET", path, nil, h.member, nil)
		v := expect(t, w, 200)
		if strings.Contains(w.Body.String(), "PRIVATE_RECORD_SECRET") || strings.Contains(w.Body.String(), "hosted_url") || strings.Contains(w.Body.String(), "payment:record-attempt") {
			t.Fatalf("private fields escaped through %s", path)
		}
		field := "items"
		if path == "/api/wallet" {
			field = "ledger"
		}
		row := v[field].([]any)[0].(map[string]any)
		payment := row["payment"].(map[string]any)
		if payment["last4"] != "4242" || payment["amount"] != float64(29999) || payment["currency"] != "jpy" || payment["charged_currency"] != "JPY" || payment["message"] != "本次付款未完成，请查看详情。" {
			t.Fatalf("bad payment summary: %#v", payment)
		}
		expect(t, h.request("GET", path, nil, nil, nil), 401)
		other := expect(t, h.request("GET", path, nil, h.other, nil), 200)
		if len(other[field].([]any)) != 0 {
			t.Fatalf("cross-customer records in %s", path)
		}
	}
	// Keep the public API and web list projection identical.
	apiKey := expect(t, h.request("POST", "/api/keys", M{"name": "records contract"}, h.member, nil), 200)["key"].(string)
	expect(t, h.request("GET", "/api/v1/orders?q="+id+"&status=draft", nil, nil, map[string]string{"Authorization": "Bearer " + apiKey}), 200)
	missing := expect(t, h.request("GET", "/api/orders?q=not-a-real-order", nil, h.member, nil), 200)
	if len(missing["items"].([]any)) != 0 {
		t.Fatal("order search ignored")
	}

	// Window calculations must include the predecessor outside the 300-row limit.
	for i := 1; i <= 301; i++ {
		exec(`INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description) VALUES($1,'adjustment',100,$2,0,$3,'测试调整')`, h.memberID, 90000+i*100, fmt.Sprintf("records:%d", i))
	}
	wallet := expect(t, h.request("GET", "/api/wallet", nil, h.member, nil), 200)
	rows := wallet["ledger"].([]any)
	if len(rows) != 300 {
		t.Fatalf("wanted 300, got %d", len(rows))
	}
	for _, r := range rows {
		m := r.(map[string]any)
		if m["available_delta"] != float64(100) || m["frozen_delta"] != float64(0) {
			t.Fatalf("limit broke history: %#v", m)
		}
	}

	// Even a malformed legacy association must not reveal another customer's order.
	otherID := expect(t, h.request("GET", "/api/me", nil, h.other, nil), 200)["id"].(string)
	exec(`INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description,order_id) VALUES($1,'freeze',0,0,0,'legacy','历史冻结',$2)`, otherID, id)
	other := expect(t, h.request("GET", "/api/wallet", nil, h.other, nil), 200)["ledger"].([]any)[0].(map[string]any)
	if other["related_order"] != nil || other["available_delta"] != nil || other["frozen_delta"] != nil || len(other["payment"].(map[string]any)) != 0 {
		t.Fatalf("unsafe legacy association: %#v", other)
	}
	exec(`UPDATE orders SET workflow='link' WHERE id=$1`, id)
	link := expect(t, h.request("GET", "/api/orders", nil, h.member, nil), 200)["items"].([]any)[0].(map[string]any)
	if len(link["payment"].(map[string]any)) != 0 {
		t.Fatal("link-only order exposed a payment")
	}
}
