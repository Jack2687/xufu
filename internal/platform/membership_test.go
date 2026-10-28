package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testSession(email string) M {
	claims, _ := json.Marshal(M{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/profile": M{"email": email}, "https://api.openai.com/auth": M{"chatgpt_account_id": "test-space"}})
	return M{"user": M{"email": email}, "account": M{"id": "test-space", "planType": "free"}, "accessToken": "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".TEST_ONLY_NOT_SIGNED", "expires": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}
}
func TestSessionInspection(t *testing.T) {
	valid := testSession("member@example.com")
	for _, tt := range []struct {
		name   string
		mutate func(M)
		want   string
	}{
		{"valid", func(M) {}, ""},
		{"missing_email", func(s M) { s["user"] = M{} }, "缺少"},
		{"email_mismatch", func(s M) { s["user"] = M{"email": "different@example.com"} }, "不一致"},
		{"account_mismatch", func(s M) { s["account"] = M{"id": "other-space"} }, "不一致"},
		{"session_expired", func(s M) { s["expires"] = "2020-01-01T00:00:00Z" }, "过期"},
		{"token_expired_even_when_session_future", func(s M) {
			c, _ := json.Marshal(M{"exp": time.Now().Add(-time.Minute).Unix()})
			s["accessToken"] = "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(c) + ".TEST"
		}, "过期"},
		{"malformed", func(s M) { s["accessToken"] = "invalid-not-a-token-at-all" }, "格式"},
		{"upstream_session_error", func(s M) { s["error"] = "RefreshAccessTokenError" }, "登录错误"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(valid)
			s := M{}
			json.Unmarshal(b, &s)
			tt.mutate(s)
			b, _ = json.Marshal(s)
			got, e := inspectSession(b)
			if tt.want != "" {
				if e == nil || !strings.Contains(e.Error(), tt.want) {
					t.Fatalf("expected %s got %v", tt.want, e)
				}
				return
			}
			if e != nil || got.Email != "member@example.com" || got.OnlineVerified || time.Until(got.ExpiresAt) > 61*time.Minute {
				t.Fatalf("inspection failed: %v", e)
			}
			quoted, _ := json.Marshal(string(b))
			if _, e = inspectSession(quoted); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestSessionOrderRetryIdentity(t *testing.T) {
	h := setupTest(t)
	session := testSession("subscription@example.com")
	request := M{"product_id": "pro", "session": session, "country": "US", "seats": 1, "accepted": true}
	headers := map[string]string{"Idempotency-Key": uid()}
	first := expect(t, h.request("POST", "/api/orders", request, h.member, headers), 200)
	// Refreshed credentials for the same account must replay the original order.
	session["accessToken"] = strings.Replace(session["accessToken"].(string), "TEST_ONLY_NOT_SIGNED", "TEST_ONLY_REFRESHED", 1)
	second := expect(t, h.request("POST", "/api/orders", request, h.member, headers), 200)
	if second["id"] != first["id"] || second["replayed"] != true {
		t.Fatal("refreshed session created a different order")
	}
	// A genuinely different account cannot reuse the order identity.
	request["session"] = testSession("different@example.com")
	expect(t, h.request("POST", "/api/orders", request, h.member, headers), 409)
	// Email is derived exclusively from Session; callers cannot override it.
	request["session"] = session
	request["email"] = "spoofed@example.com"
	expect(t, h.request("POST", "/api/orders", request, h.member, map[string]string{"Idempotency-Key": uid()}), 400)
	var count int
	if err := h.a.DB.QueryRow(context.Background(), "SELECT count(*) FROM orders WHERE user_id=$1", h.memberID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected exactly one order, count=%d error=%v", count, err)
	}
	detail := expect(t, h.request("GET", "/api/orders/"+first["id"].(string), nil, h.member, nil), 200)
	if detail["order"].(map[string]any)["email"] != "subscription@example.com" {
		t.Fatal("order email did not come from Session")
	}
}

func TestMembershipLifecycleAndAPIs(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	get := func() Membership {
		m, e := membershipFor(ctx, h.a.DB, h.memberID)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	if m := get(); m.Current.Code != "L1" || len(m.Tiers) != 6 || m.Remaining != 200000 {
		t.Fatalf("defaults: %+v", m)
	}
	// Recharge, redemption, manual adjustment and unlinked charges never grant growth.
	for _, kind := range []string{"topup", "redeem", "adjustment", "charge"} {
		amount := int64(1000000)
		if kind == "charge" {
			amount = -amount
		}
		_, e := h.a.DB.Exec(ctx, "INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description) VALUES($1,$2,$3,0,0,$4,'test')", h.memberID, kind, amount, uid())
		if e != nil {
			t.Fatal(e)
		}
	}
	if get().Spent != 0 {
		t.Fatal("nonsettled funds counted as growth")
	}
	id := h.draft(uid())["id"].(string)
	_, e := h.a.DB.Exec(ctx, "INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description,order_id) VALUES($1,'charge',-200000,0,0,$2,'test',$3)", h.memberID, uid(), id)
	if e != nil {
		t.Fatal(e)
	}
	if get().Spent != 0 {
		t.Fatal("draft charge counted")
	}
	if _, e = h.a.DB.Exec(ctx, "UPDATE orders SET status='failed' WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	if get().Spent != 0 {
		t.Fatal("failed order counted")
	}
	if _, e = h.a.DB.Exec(ctx, "UPDATE orders SET status='succeeded' WHERE id=$1", id); e != nil {
		t.Fatal(e)
	}
	if m := get(); m.Current.Code != "L2" || m.Spent != 200000 || m.Remaining != 300000 {
		t.Fatalf("upgrade failed %+v", m)
	}
	quote := expect(t, h.request("GET", "/api/quote?product_id=pro", nil, h.member, nil), 200)
	if quote["fee"] != float64(9500) {
		t.Fatal("member price", quote)
	}
	draft := h.draft(uid())["id"].(string)
	cfg := defaultTiers()
	cfg.Items[1].Discount = 94
	expect(t, h.request("PUT", "/api/admin/settings/tiers", cfg, h.admin, nil), 200)
	detail := expect(t, h.request("GET", "/api/orders/"+draft, nil, h.member, nil), 200)["order"].(map[string]any)
	if detail["fee"] != float64(9500) || detail["price_snapshot"].(map[string]any)["discount"] != float64(95) {
		t.Fatal("price snapshot changed")
	}
	expect(t, h.request("PUT", "/api/admin/users/"+h.memberID+"/membership", M{"code": "L5", "until": nil, "reason": "test promotion"}, h.member, nil), 403)
	expect(t, h.request("PUT", "/api/admin/users/"+h.memberID+"/membership", M{"code": "L5", "until": nil, "reason": "test promotion"}, h.admin, nil), 200)
	if m := get(); m.Current.Code != "L5" || m.Automatic.Code != "L2" || m.Source != "manual" {
		t.Fatal("override")
	}
	without := defaultTiers()
	without.Items = append(without.Items[:4], without.Items[5:]...)
	expect(t, h.request("PUT", "/api/admin/settings/tiers", without, h.admin, nil), 400)
	if _, e = h.a.DB.Exec(ctx, "UPDATE users SET membership_until=now()-interval '1 second' WHERE id=$1", h.memberID); e != nil {
		t.Fatal(e)
	}
	if m := get(); m.Current.Code != "L2" || m.Source != "automatic" {
		t.Fatal("expired override")
	}
	expect(t, h.request("PUT", "/api/admin/users/"+h.memberID+"/membership", M{"code": "", "reason": "restore automatic"}, h.admin, nil), 200)
	_, e = h.a.DB.Exec(ctx, "INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description,order_id) VALUES($1,'refund',1,0,0,$2,'test',$3)", h.memberID, uid(), id)
	if e != nil {
		t.Fatal(e)
	}
	if m := get(); m.Spent != 199999 || m.Current.Code != "L1" {
		t.Fatal("refund growth not reversed")
	}
	if _, e = h.a.DB.Exec(ctx, "UPDATE users SET fee_override=777 WHERE id=$1", h.memberID); e != nil {
		t.Fatal(e)
	}
	if fee, e := priceFor(ctx, h.a.DB, h.memberID, 10000); e != nil || fee != 777 {
		t.Fatal("override fee", e, fee)
	}
	member := expect(t, h.request("GET", "/api/membership", nil, h.member, nil), 200)
	if len(member["growth"].([]any)) != 1 {
		t.Fatal("growth list")
	}
	report := expect(t, h.request("GET", "/api/admin/membership", nil, h.admin, nil), 200)
	if report["customers"] != float64(2) {
		t.Fatal("admin counted")
	}
	for _, cookie := range []*http.Cookie{nil, h.other} {
		expected := 403
		if cookie == nil {
			expected = 401
		}
		expect(t, h.request("GET", "/api/admin/membership", nil, cookie, nil), expected)
	}
	// Public specification documents real routes; keys can only read their own data.
	keyResult := expect(t, h.request("POST", "/api/keys", M{"name": "member-contract"}, h.member, nil), 200)
	headers := map[string]string{"Authorization": "Bearer " + keyResult["key"].(string)}
	for _, route := range []string{"membership", "catalog", "wallet", "quote?product_id=pro"} {
		expect(t, h.request("GET", "/api/v1/"+route, nil, nil, headers), 200)
	}
	session := testSession("subscription@example.com")
	w := h.request("POST", "/api/v1/session/inspect", M{"session": session}, nil, headers)
	expect(t, w, 200)
	if strings.Contains(w.Body.String(), session["accessToken"].(string)) {
		t.Fatal("token reflected")
	}
	expect(t, h.request("GET", "/api/admin/membership", nil, nil, headers), 403)
	spec := expect(t, h.request("GET", "/api/openapi.json", nil, nil, nil), 200)
	if len(spec["paths"].(map[string]any)) != 12 {
		t.Fatal("contract routes missing")
	}
	// Raw session/token never persists in orders, events, audit or risk rows.
	for _, table := range []string{"orders", "order_events", "audit_events", "risk_events"} {
		var leaked bool
		sql := "SELECT EXISTS(SELECT 1 FROM " + table + " t WHERE row_to_json(t)::text LIKE '%TEST_ONLY_NOT_SIGNED%')"
		if e = h.a.DB.QueryRow(ctx, sql).Scan(&leaked); e != nil || leaked {
			t.Fatalf("credential persistence in %s: %v", table, e)
		}
	}
}
