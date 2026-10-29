package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type harness struct {
	t                    *testing.T
	a                    *App
	h                    http.Handler
	admin, member, other *http.Cookie
	memberID             string
}

func setupTest(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL needed for isolated PostgreSQL integration")
	}
	ctx := context.Background()
	db, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schemaName := "xupay_test_" + uid()
	if _, e = db.Exec(ctx, "CREATE SCHEMA "+schemaName); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	t.Setenv("BOOTSTRAP_EMAIL", "admin@test.local")
	t.Setenv("BOOTSTRAP_PASSWORD", "LocalTestOnly-LongPassword")
	app, e := New(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)), "../../webui", false)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		app.DB.Close()
		if strings.HasPrefix(schemaName, "xupay_test_") {
			db.Exec(ctx, "DROP SCHEMA "+schemaName+" CASCADE")
		}
		db.Close()
	})
	// Most protocol fixtures exercise the full new flow explicitly. Installation
	// defaults are tested separately from the fixture execution choice.
	if _, e = app.DB.Exec(ctx, `UPDATE settings SET value=value || '{"checkout_flow":"cslive","subscription_flow":"cslive"}'::jsonb WHERE key='payment'`); e != nil {
		t.Fatal(e)
	}
	h := &harness{t: t, a: app, h: app.Handler()}
	h.admin = h.login("admin@test.local", "LocalTestOnly-LongPassword")
	h.member = h.register("member@test.local")
	h.other = h.register("other@test.local")
	w := h.request("GET", "/api/me", nil, h.member, nil)
	h.memberID = decode(t, w)["id"].(string)
	return h
}
func (h *harness) request(method, path string, body any, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.RemoteAddr = "203.0.113.40:1234"
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}
func decode(t *testing.T, w *httptest.ResponseRecorder) M {
	t.Helper()
	v := M{}
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatalf("invalid JSON code=%d", w.Code)
	}
	return v
}
func expect(t *testing.T, w *httptest.ResponseRecorder, status int) M {
	t.Helper()
	if w.Code != status {
		t.Fatalf("wanted %d got %d: %s", status, w.Code, w.Body.String())
	}
	return decode(t, w)
}
func (h *harness) login(email, pw string) *http.Cookie {
	w := h.request("POST", "/api/auth/login", M{"email": email, "password": pw}, nil, nil)
	expect(h.t, w, 200)
	return w.Result().Cookies()[0]
}
func (h *harness) register(email string) *http.Cookie {
	w := h.request("POST", "/api/auth/register", M{"email": email, "password": "CustomerTestPassword123", "name": "测试客户", "invite": ""}, nil, nil)
	expect(h.t, w, 200)
	return w.Result().Cookies()[0]
}
func (h *harness) resetRates() { h.a.DB.Exec(context.Background(), "DELETE FROM rate_buckets") }
func (h *harness) draft(key string) M {
	return expect(h.t, h.request("POST", "/api/orders", M{"product_id": "pro", "session": testSession("subscription@example.com"), "country": "PH", "seats": 1, "accepted": true}, h.member, map[string]string{"Idempotency-Key": key}), 200)
}

func TestPlatformIntegration(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	t.Run("anonymous_and_member_admin_boundaries", func(t *testing.T) {
		for _, p := range []string{"overview", "users", "orders", "finance", "topups", "codes", "resources", "risk", "settings", "announcements", "audit", "products"} {
			expect(t, h.request("GET", "/api/admin/"+p, nil, h.admin, nil), 200)
			expect(t, h.request("GET", "/api/admin/"+p, nil, nil, nil), 401)
			expect(t, h.request("GET", "/api/admin/"+p, nil, h.member, nil), 403)
		}
		expect(t, h.request("POST", "/api/admin/users", M{}, h.member, nil), 403)
		w := h.request("GET", "/api/me", nil, h.member, nil)
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("CSP missing")
		}
	})
	t.Run("csrf_and_input_boundaries", func(t *testing.T) {
		expect(t, h.request("POST", "/api/topups", M{"amount": 100}, h.member, map[string]string{"Origin": "https://evil.example"}), 403)
		expect(t, h.request("POST", "/api/topups", M{"amount": 100, "role": "admin"}, h.member, nil), 400)
		expect(t, h.request("POST", "/api/orders", M{"product_id": "pro", "session": testSession("subscription@example.com"), "country": "PH", "seats": 1, "accepted": true, "access_token": "must-not-be-accepted"}, h.member, map[string]string{"Idempotency-Key": uid()}), 400)
	})
	t.Run("adjustments_idempotent_and_no_overdraft", func(t *testing.T) {
		key := uid()
		headers := map[string]string{"Idempotency-Key": key}
		body := M{"amount": 10000, "reason": "测试充值调整"}
		w := h.request("POST", "/api/admin/users/"+h.memberID+"/balance", body, h.admin, headers)
		expect(t, w, 200)
		expect(t, h.request("POST", "/api/admin/users/"+h.memberID+"/balance", body, h.admin, headers), 200)
		expect(t, h.request("POST", "/api/admin/users/"+h.memberID+"/balance", M{"amount": 20000, "reason": "不同内容"}, h.admin, headers), 409)
		expect(t, h.request("POST", "/api/admin/users/"+h.memberID+"/balance", M{"amount": -10001, "reason": "余额不足测试"}, h.admin, map[string]string{"Idempotency-Key": uid()}), 400)
		var bal, n int64
		h.a.DB.QueryRow(ctx, "SELECT balance FROM users WHERE id=$1", h.memberID).Scan(&bal)
		h.a.DB.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE user_id=$1", h.memberID).Scan(&n)
		if bal != 10000 || n != 1 {
			t.Fatalf("balance=%d entries=%d", bal, n)
		}
	})
	t.Run("parallel_adjustments_do_not_lose_updates", func(t *testing.T) {
		var wg sync.WaitGroup
		codes := make(chan int, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := h.request("POST", "/api/admin/users/"+h.memberID+"/balance", M{"amount": 100, "reason": "并发调整测试"}, h.admin, map[string]string{"Idempotency-Key": uid()})
				codes <- w.Code
			}()
		}
		wg.Wait()
		close(codes)
		for c := range codes {
			if c != 200 {
				t.Fatal(c)
			}
		}
		var b int64
		h.a.DB.QueryRow(ctx, "SELECT balance FROM users WHERE id=$1", h.memberID).Scan(&b)
		if b != 11200 {
			t.Fatal(b)
		}
	})
	t.Run("concurrent_redemption_exactly_once", func(t *testing.T) {
		v := expect(t, h.request("POST", "/api/admin/codes", M{"count": 1, "amount": 5000, "days": 30}, h.admin, nil), 200)
		code := v["codes"].([]any)[0].(string)
		var wg sync.WaitGroup
		results := make(chan int, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- h.request("POST", "/api/wallet/redeem", M{"code": code}, h.member, nil).Code
			}()
		}
		wg.Wait()
		close(results)
		ok := 0
		for c := range results {
			if c == 200 {
				ok++
			} else if c != 400 {
				t.Fatal(c)
			}
		}
		if ok != 1 {
			t.Fatalf("successes=%d", ok)
		}
		var enc string
		h.a.DB.QueryRow(ctx, "SELECT code_enc FROM redeem_codes WHERE code_hash=$1", hash(code)).Scan(&enc)
		if strings.Contains(enc, code) {
			t.Fatal("plaintext code")
		}
		var bal int64
		h.a.DB.QueryRow(ctx, "SELECT balance FROM users WHERE id=$1", h.memberID).Scan(&bal)
		if bal != 16200 {
			t.Fatal(bal)
		}
	})
	t.Run("topup_review_race_exactly_once", func(t *testing.T) {
		v := expect(t, h.request("POST", "/api/topups", M{"amount": 9000, "proof": "test reference"}, h.member, nil), 200)
		id := v["id"].(string)
		var wg sync.WaitGroup
		results := make(chan int, 6)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- h.request("POST", "/api/admin/topups/"+id+"/review", M{"action": "approve", "note": "测试已核对到账"}, h.admin, nil).Code
			}()
		}
		wg.Wait()
		close(results)
		for c := range results {
			if c != 200 {
				t.Fatal(c)
			}
		}
		var n int
		h.a.DB.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE reference=$1", "topup:"+id).Scan(&n)
		if n != 1 {
			t.Fatal(n)
		}
		expect(t, h.request("POST", "/api/topups/"+id+"/cancel", M{}, h.member, nil), 409)
		var b int64
		h.a.DB.QueryRow(ctx, "SELECT balance FROM users WHERE id=$1", h.memberID).Scan(&b)
		if b != 25200 {
			t.Fatal(b)
		}
	})
	t.Run("order_idempotency_and_tenant_isolation", func(t *testing.T) {
		h.resetRates()
		idKey := uid()
		v := h.draft(idKey)
		id := v["id"].(string)
		second := h.draft(idKey)
		if second["id"] != id {
			t.Fatal("duplicate order")
		}
		expect(t, h.request("POST", "/api/orders", M{"product_id": "pro5x", "session": testSession("subscription@example.com"), "country": "PH", "seats": 1, "accepted": true}, h.member, map[string]string{"Idempotency-Key": idKey}), 409)
		expect(t, h.request("GET", "/api/orders/"+id, nil, h.other, nil), 404)
		expect(t, h.request("POST", "/api/orders/"+id+"/cancel", M{}, h.other, nil), 404)
		h.a.DB.Exec(ctx, "UPDATE orders SET note='INTERNAL_NOTE_NOT_FOR_CUSTOMER' WHERE id=$1", id)
		w := h.request("GET", "/api/orders/"+id, nil, h.member, nil)
		expect(t, w, 200)
		for _, s := range []string{"INTERNAL_NOTE", "request_hash", "idempotency_key"} {
			if strings.Contains(w.Body.String(), s) {
				t.Fatalf("leaked %s", s)
			}
		}
		expect(t, h.request("POST", "/api/orders/"+id+"/cancel", M{}, h.member, nil), 200)
		expect(t, h.request("POST", "/api/orders/"+id+"/cancel", M{}, h.member, nil), 404)
		var b int64
		h.a.DB.QueryRow(ctx, "SELECT balance FROM users WHERE id=$1", h.memberID).Scan(&b)
		if b != 25200 {
			t.Fatal("draft charged")
		}
	})
	t.Run("api_key_scoping_and_revoke", func(t *testing.T) {
		v := expect(t, h.request("POST", "/api/keys", M{"name": "integration"}, h.member, nil), 200)
		headers := map[string]string{"Authorization": "Bearer " + v["key"].(string)}
		expect(t, h.request("GET", "/api/v1/wallet", nil, nil, headers), 200)
		expect(t, h.request("GET", "/api/admin/users", nil, nil, headers), 403)
		expect(t, h.request("GET", "/api/me", nil, nil, headers), 403)
		w := h.request("GET", "/api/keys", nil, h.member, nil)
		if strings.Contains(w.Body.String(), v["key"].(string)) {
			t.Fatal("key exposed after creation")
		}
		expect(t, h.request("DELETE", "/api/keys/"+v["id"].(string), nil, h.member, nil), 200)
		expect(t, h.request("GET", "/api/v1/wallet", nil, nil, headers), 401)
	})
	t.Run("resources_encrypted_and_internal_addresses_blocked", func(t *testing.T) {
		expect(t, h.request("POST", "/api/admin/resources", M{"name": "invalid", "endpoint": "127.0.0.1:8000:user:password", "country": "US", "enabled": true, "weight": 1, "capacity": 1}, h.admin, nil), 400)
		v := expect(t, h.request("POST", "/api/admin/resources", M{"name": "test resource", "endpoint": "proxy.example.com:8000:user:secret-test-value", "country": "US", "enabled": true, "weight": 1, "capacity": 1}, h.admin, nil), 200)
		w := h.request("GET", "/api/admin/resources", nil, h.admin, nil)
		if strings.Contains(w.Body.String(), "secret-test-value") || strings.Contains(w.Body.String(), "endpoint_enc") || strings.Contains(w.Body.String(), "proxy.example.com") {
			t.Fatal("resource credentials exposed")
		}
		var enc string
		h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM resources WHERE id=$1", v["id"]).Scan(&enc)
		if strings.Contains(enc, "secret-test-value") {
			t.Fatal("unencrypted credential")
		}
		raw, e := h.a.decrypt(enc)
		if e != nil || !strings.Contains(raw, "secret-test-value") {
			t.Fatal("credential roundtrip failed")
		}
	})
	t.Run("announcement_visibility_and_xss_is_text", func(t *testing.T) {
		expect(t, h.request("POST", "/api/admin/announcements", M{"title": "private test", "body": "not for login", "tone": "info", "published": true, "login_visible": false, "sort": 0}, h.admin, nil), 200)
		expect(t, h.request("POST", "/api/admin/announcements", M{"title": "public test", "body": "<script>alert(1)</script>", "tone": "info", "published": true, "login_visible": true, "sort": 0}, h.admin, nil), 200)
		w := h.request("GET", "/api/public", nil, nil, nil)
		expect(t, w, 200)
		if strings.Contains(w.Body.String(), "not for login") {
			t.Fatal("private announcement leaked")
		}
		if strings.Contains(w.Body.String(), "<script>") {
			t.Fatal("JSON HTML not escaped")
		}
	})
	t.Run("finance_real_ledger_and_admin_excluded", func(t *testing.T) {
		v := expect(t, h.request("GET", "/api/admin/finance?period=day", nil, h.admin, nil), 200)
		tot := v["totals"].(map[string]any)
		if tot["credited"] != float64(14000) || tot["spent"] != float64(0) || tot["adjusted"] != float64(11200) {
			t.Fatalf("totals %v", tot)
		}
		if len(v["users"].([]any)) != 2 {
			t.Fatal("admin included")
		}
		w := h.request("GET", "/api/admin/finance/export?period=month", nil, h.admin, nil)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/csv") {
			t.Fatal("export failed")
		}
		expect(t, h.request("GET", "/api/admin/finance?start=bad", nil, h.admin, nil), 400)
	})
	t.Run("validation_no_accidental_engine_enable", func(t *testing.T) {
		cfg := h.a.config(ctx, "scheduler")
		cfg["engine"] = "live"
		expect(t, h.request("PUT", "/api/admin/settings/scheduler", cfg, h.admin, nil), 400)
		cfg["engine"] = "not_connected"
		cfg["max_parallel"] = float64(0)
		expect(t, h.request("PUT", "/api/admin/settings/scheduler", cfg, h.admin, nil), 400)
		cfg["max_parallel"] = float64(3)
		expect(t, h.request("PUT", "/api/admin/settings/scheduler", cfg, h.admin, nil), 200)
		tiers := M{"items": []any{M{"code": "L1", "name": "标准", "threshold": 0, "discount": 95.5}}}
		expect(t, h.request("PUT", "/api/admin/settings/tiers", tiers, h.admin, nil), 400)
		tiers["items"].([]any)[0].(M)["discount"] = 95
		expect(t, h.request("PUT", "/api/admin/settings/tiers", tiers, h.admin, nil), 200)
		fee, err := priceFor(ctx, h.a.DB, h.memberID, 1234)
		if err != nil || fee != 1172 {
			t.Fatalf("saved tier quotation: fee=%d err=%v", fee, err)
		}
		badCfg := h.a.config(ctx, "risk")
		badCfg["unknown"] = "test"
		expect(t, h.request("PUT", "/api/admin/settings/risk", badCfg, h.admin, nil), 400)
	})
	t.Run("login_ratelimit_uses_peer_ip_not_forged_header", func(t *testing.T) {
		h.resetRates()
		for i := 0; i < 10; i++ {
			expect(t, h.request("POST", "/api/auth/login", M{"email": "nobody@test.local", "password": "wrong"}, nil, map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i+1)}), 401)
		}
		expect(t, h.request("POST", "/api/auth/login", M{"email": "nobody@test.local", "password": "wrong"}, nil, map[string]string{"X-Forwarded-For": "8.8.8.8"}), 429)
		var actual string
		h.a.DB.QueryRow(ctx, "SELECT ip FROM risk_events WHERE category='login_rate' ORDER BY id DESC LIMIT 1").Scan(&actual)
		if actual != "203.0.113.40" {
			t.Fatal(actual)
		}
	})
	t.Run("disabled_user_loses_access", func(t *testing.T) {
		h.resetRates()
		u := expect(t, h.request("GET", "/api/me", nil, h.other, nil), 200)
		expect(t, h.request("PATCH", "/api/admin/users/"+u["id"].(string), M{"email": "other@test.local", "name": "other", "role": "member", "status": "disabled", "fee_override": nil, "password": ""}, h.admin, nil), 200)
		expect(t, h.request("GET", "/api/me", nil, h.other, nil), 401)
	})
	t.Run("cleanup_maintenance_does_not_touch_account_data", func(t *testing.T) {
		h.a.DB.Exec(ctx, "INSERT INTO rate_buckets(key,hits,expires_at) VALUES('expired-test',1,now()-interval '1 day')")
		h.a.DB.Exec(ctx, "DELETE FROM rate_buckets WHERE expires_at<now()-interval '1 hour'")
		var n int
		h.a.DB.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n)
		if n != 3 {
			t.Fatal(n)
		}
	})
}

func TestQueueLeasesAndSafety(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	h.a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{max_parallel}','2') WHERE key='scheduler'")
	add := func(user string) string {
		id := uid()
		_, e := h.a.DB.Exec(ctx, "INSERT INTO orders(id,user_id,product_id,product_name,email,fee,idempotency_key,request_hash,status) VALUES($1,$2,'pro','Pro','job@example.com',0,$1,$1,'queued')", id, user)
		if e != nil {
			t.Fatal(e)
		}
		_, e = h.a.DB.Exec(ctx, "INSERT INTO jobs(id,order_id,user_id) VALUES($1,$1,$2)", id, user)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	member2 := expect(t, h.request("GET", "/api/me", nil, h.other, nil), 200)["id"].(string)
	add(h.memberID)
	add(h.memberID)
	add(member2)
	t.Run("parallel_claim_enforces_global_and_per_user", func(t *testing.T) {
		var wg sync.WaitGroup
		ch := make(chan *Job, 8)
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				j, e := h.a.Claim(ctx)
				if e != nil {
					errs <- e
				}
				if j != nil {
					ch <- j
				}
			}()
		}
		wg.Wait()
		close(ch)
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		claims := []*Job{}
		for j := range ch {
			claims = append(claims, j)
		}
		if len(claims) != 2 || claims[0].UserID == claims[1].UserID {
			t.Fatalf("claim count %d", len(claims))
		}
		for _, j := range claims {
			if e := h.a.Finish(ctx, j, false, true, "network"); e != nil {
				t.Fatal(e)
			}
		}
		j, e := h.a.Claim(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if j == nil {
			t.Fatal("other eligible FIFO task should proceed")
		}
		h.a.Finish(ctx, j, false, false, "invalid_credentials")
		j, e = h.a.Claim(ctx)
		if e != nil || j != nil {
			t.Fatal("delayed jobs claimed before retry interval")
		}
	})
	t.Run("uncertain_side_effect_not_retried_and_stale_lease_denied", func(t *testing.T) {
		h.a.DB.Exec(ctx, "UPDATE jobs SET next_at=now()-interval '1 second' WHERE state='queued'")
		j, e := h.a.Claim(ctx)
		if e != nil || j == nil {
			t.Fatal("claim failed")
		}
		if e = h.a.BeforeSideEffect(ctx, j); e != nil {
			t.Fatal(e)
		}
		h.a.DB.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", j.ID)
		if e = h.a.Heartbeat(ctx, j); e != ErrLeaseLost {
			t.Fatal("stale heartbeat accepted")
		}
		if e = h.a.Finish(ctx, j, true, false, "done"); e != ErrLeaseLost {
			t.Fatal("stale completion accepted")
		}
		h.a.Claim(ctx)
		var state string
		h.a.DB.QueryRow(ctx, "SELECT state FROM jobs WHERE id=$1", j.ID).Scan(&state)
		if state != "review" {
			t.Fatal("uncertain payment was not isolated")
		}
	})
	t.Run("paused_queue_does_not_claim", func(t *testing.T) {
		h.a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{paused}','true') WHERE key='scheduler'")
		j, e := h.a.Claim(ctx)
		if e != nil || j != nil {
			t.Fatal("paused queue claimed")
		}
	})
}

func TestProxyNormalizationAndEncryption(t *testing.T) {
	for _, s := range []string{"127.0.0.1:8000:a:b", "http://169.254.169.254:80", "http://localhost:80/path", "file:///etc/passwd", "bad:port"} {
		if _, e := normalizeProxy(s); e == nil {
			t.Errorf("accepted %s", s)
		}
	}
	s, e := normalizeProxy("proxy.example.com:8080:user:pass")
	if e != nil || s != "http://user:pass@proxy.example.com:8080" {
		t.Fatal(s, e)
	}
	a := &App{Key: bytes.Repeat([]byte{7}, 32)}
	c, e := a.encrypt("secret")
	if e != nil {
		t.Fatal(e)
	}
	p, e := a.decrypt(c)
	if e != nil || p != "secret" {
		t.Fatal("roundtrip")
	}
	a.Key = bytes.Repeat([]byte{8}, 32)
	if _, e = a.decrypt(c); e == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestCSVInjectionEscaped(t *testing.T) {
	for _, s := range []string{"=1+1", "+cmd", "-cmd", "@sum", "\tformula"} {
		if !strings.HasPrefix(csvSafe(s), "'") {
			t.Fatal(s)
		}
	}
}

var _ = time.Now
