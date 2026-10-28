package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type testRoundTrip func(*http.Request) (*http.Response, error)

func (f testRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func providerResponse(code int, v M) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}
}

func TestCheckoutProviderProtocol(t *testing.T) {
	for _, product := range []string{"pro", "pro5x", "team", "team_premium"} {
		t.Run(product, func(t *testing.T) {
			in := checkoutInput{AccessToken: "QA_PRIVATE_TOKEN", AccountID: "SPACE", Product: product, Country: "PH", Mode: "long", Workspace: "Test team", Seats: 2, PremiumSeats: 3}
			writes, calls := 0, 0
			client := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host == "ipinfo.io" {
					if r.Header.Get("Authorization") != "" {
						t.Fatal("token sent to proxy probe")
					}
					return providerResponse(200, M{"country": "PH", "ip": "203.0.113.2"}), nil
				}
				if r.URL.String() != checkoutEndpoint || r.Method != "POST" || writes != 1 {
					t.Fatal("unexpected checkout protocol")
				}
				if r.Header.Get("Authorization") != "Bearer QA_PRIVATE_TOKEN" || r.Header.Get("ChatGPT-Account-ID") != "SPACE" || r.Header.Get("User-Agent") != iosCheckoutUA {
					t.Fatal("checkout authorization or context mismatch")
				}
				var body M
				json.NewDecoder(r.Body).Decode(&body)
				if body["billing_details"].(map[string]any)["country"] != "PH" || body["checkout_ui_mode"] != "hosted" {
					t.Fatal("billing region or mode changed")
				}
				if _, exists := body["entry_point"]; exists {
					t.Fatal("hosted mode must omit entry_point like generator.py")
				}
				if product == "team_premium" {
					team := body["team_plan_data"].(map[string]any)
					if len(team["seat_quantities"].([]any)) != 2 {
						t.Fatal("premium seats missing")
					}
				}
				return providerResponse(200, M{"checkout_session_id": "cs_test_123", "url": "https://pay.openai.com/c/pay/cs_test_123", "tag": "hosted_checkout_session", "checkout_ui_mode": "hosted"}), nil
			})}
			result, e := generateCheckout(context.Background(), client, in, func() error { writes++; return nil })
			if e != nil || calls != 2 || result.URL != "https://pay.openai.com/c/pay/cs_test_123" {
				t.Fatalf("checkout protocol failed: %v", e)
			}
		})
	}
}

func TestCheckoutNoUnsafeRetryOrDirectFallback(t *testing.T) {
	in := checkoutInput{AccessToken: "QA_PRIVATE_TOKEN", AccountID: "SPACE", Product: "pro", Country: "PH", Mode: "long"}
	for _, mode := range []string{"country_mismatch", "network_before_write", "network_after_write", "unauthorized", "malicious_url", "rate_limited"} {
		t.Run(mode, func(t *testing.T) {
			writes := 0
			client := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "ipinfo.io" {
					if mode == "country_mismatch" {
						return providerResponse(200, M{"country": "US"}), nil
					}
					if mode == "network_before_write" {
						return nil, errors.New("QA_PRIVATE_TOKEN proxy-password")
					}
					return providerResponse(200, M{"country": "PH"}), nil
				}
				switch mode {
				case "network_after_write":
					return nil, errors.New("QA_PRIVATE_TOKEN")
				case "unauthorized":
					return providerResponse(401, M{"error": "QA_PRIVATE_TOKEN"}), nil
				case "rate_limited":
					return providerResponse(429, M{}), nil
				default:
					return providerResponse(200, M{"checkout_session_id": "cs_test", "url": "http://127.0.0.1:80/?secret=QA_PRIVATE_TOKEN"}), nil
				}
			})}
			result, e := generateCheckout(context.Background(), client, in, func() error { writes++; return nil })
			var failure checkoutError
			if !errors.As(e, &failure) || result.URL != "" || strings.Contains(e.Error(), "QA_PRIVATE_TOKEN") {
				t.Fatal("unsafe provider error result")
			}
			if mode == "country_mismatch" || mode == "network_before_write" {
				if writes != 0 || !failure.Retry || failure.Unknown {
					t.Fatal("preflight retry classification")
				}
			}
			if mode == "network_after_write" || mode == "malicious_url" {
				if !failure.Unknown || failure.Retry {
					t.Fatal("unknown write was retryable")
				}
			}
			if mode == "unauthorized" && (failure.Unknown || failure.Retry) {
				t.Fatal("unauthorized retry")
			}
		})
	}
	if _, _, e := newPaymentClient("", time.Second); e == nil {
		t.Fatal("empty proxy accepted")
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		client, closeClient, e := newPaymentClient(scheme+"://user:password@proxy.example.com:8080", time.Second)
		if e != nil {
			t.Fatal(e)
		}
		transport := client.Transport.(proxyOnlyTransport)
		called := 0
		transport.transport.DialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
			called++
			if addr != "proxy.example.com:8080" {
				t.Fatal("attempted a direct dial")
			}
			return nil, errors.New("proxy unavailable")
		}
		_, e = client.Get("https://ipinfo.io/json")
		if e == nil || called != 1 {
			t.Fatal("proxy dial not enforced")
		}
		called = 0
		_, e = client.Get("https://other.example/path")
		if e == nil || called != 0 {
			t.Fatal("unapproved target was dialed")
		}
		closeClient()
	}
}

func prepareRequest() M {
	return M{"product_id": "pro", "session": testSession("subscription@example.com"), "country": "PH", "seats": 1, "accepted": true, "prepare": true, "workspace_name": "Test", "premium_seats": 1}
}
func (h *harness) addProxy(country string, capacity int) string {
	r := expect(h.t, h.request("POST", "/api/admin/resources", M{"name": "QA proxy", "country": country, "enabled": true, "weight": 1, "capacity": capacity, "endpoint": "proxy.example.com:8080:user:QA_PROXY_PASSWORD"}, h.admin, nil), 200)
	return r["id"].(string)
}

func TestCheckoutQueueAndIsolation(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	// An available proxy from another country must not start this PH order.
	otherRegion := h.addProxy("US", 1)
	expect(t, h.request("PUT", "/api/admin/settings/proxy", M{"default_id": otherRegion, "match_country": true, "request_timeout_seconds": 60}, h.admin, nil), 200)
	input := prepareRequest()
	if str(h.a.config(ctx, "scheduler"), "link_mode") != "long" {
		t.Fatal("checkout must default to hosted long links")
	}
	headers := map[string]string{"Idempotency-Key": uid()}
	first := expect(t, h.request("POST", "/api/orders", input, h.member, headers), 200)
	if first["status"] != "queued" {
		t.Fatal("not queued")
	}
	id := first["id"].(string)
	second := expect(t, h.request("POST", "/api/orders", input, h.member, headers), 200)
	if second["id"] != id || second["replayed"] != true {
		t.Fatal("duplicate preparation")
	}
	var credentials string
	if e := h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM checkout_runs WHERE order_id=$1", id).Scan(&credentials); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(input["session"])
	if strings.Contains(credentials, string(raw)) || credentials == "" {
		t.Fatal("unencrypted session")
	}
	job, e := h.a.claim(ctx, "checkout")
	if e != nil || job != nil {
		t.Fatal("claimed without proxy")
	}
	var attempts int
	var code string
	h.a.DB.QueryRow(ctx, "SELECT attempts,error_code FROM jobs WHERE order_id=$1", id).Scan(&attempts, &code)
	if attempts != 0 || code != "waiting_proxy" {
		t.Fatal("proxy waiting consumed retry")
	}
	detail := expect(t, h.request("GET", "/api/orders/"+id, nil, h.member, nil), 200)
	if !strings.Contains(detail["checkout"].(map[string]any)["waiting_message"].(string), "代理") {
		t.Fatal("waiting reason missing")
	}
	if _, exposed := detail["order"].(map[string]any)["billing_address"]; exposed {
		t.Fatal("billing data exposed in checkout details")
	}
	var storedBilling M
	if e = h.a.DB.QueryRow(ctx, "SELECT billing_address FROM orders WHERE id=$1", id).Scan(&storedBilling); e != nil || len(storedBilling) != 0 {
		t.Fatal("checkout unnecessarily generated billing data")
	}
	// Old stored addresses must also stay out of customer responses.
	if _, e = h.a.DB.Exec(ctx, `UPDATE orders SET billing_address='{"first_name":"QA_PRIVATE_BILLING"}' WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(h.request("GET", "/api/orders/"+id, nil, h.member, nil).Body.String(), "QA_PRIVATE_BILLING") {
		t.Fatal("legacy billing data exposed")
	}
	expect(t, h.request("GET", "/api/orders/"+id, nil, h.other, nil), 404)
	proxy := h.addProxy("PH", 1)
	h.a.DB.Exec(ctx, "UPDATE jobs SET next_at=now() WHERE order_id=$1", id)
	job, e = h.a.claim(ctx, "checkout")
	if e != nil || job == nil {
		t.Fatalf("claim: %v", e)
	}
	expect(t, h.request("DELETE", "/api/admin/resources/"+proxy, nil, h.admin, nil), 409)
	if e = h.a.BeforeSideEffect(ctx, job); e != nil {
		t.Fatal(e)
	}
	if e = h.a.finishCheckout(ctx, job, checkoutOutput{URL: "https://pay.openai.com/c/pay/cs_test_owned"}, nil); e != nil {
		t.Fatal(e)
	}
	detail = expect(t, h.request("GET", "/api/orders/"+id, nil, h.member, nil), 200)
	if detail["order"].(map[string]any)["status"] != "awaiting_payment" || detail["checkout"].(map[string]any)["ready"] != true {
		t.Fatal("link not available")
	}
	if detail["order"].(M)["workflow"] != "link" || detail["payment"].(M)["enabled"] != false || detail["checkout"].(M)["payment_connected"] != false {
		t.Fatal("link-only order exposed a payment flow")
	}
	expect(t, h.request("POST", "/api/orders/"+id+"/payment/prepare", paymentPrepareBody(), h.member, nil), 409)
	var paymentCount int
	if e = h.a.DB.QueryRow(ctx, "SELECT count(*) FROM payment_runs WHERE order_id=$1", id).Scan(&paymentCount); e != nil || paymentCount != 0 {
		t.Fatal("link generation created payment work", e)
	}
	h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM checkout_runs WHERE order_id=$1", id).Scan(&credentials)
	if credentials != "" {
		t.Fatal("credential retained after completion")
	}
	for _, path := range []string{"/api/orders/" + id, "/api/admin/orders/" + id, "/api/admin/resources"} {
		cookie := h.admin
		if !strings.Contains(path, "admin") {
			cookie = h.member
		}
		w := h.request("GET", path, nil, cookie, nil)
		if strings.Contains(w.Body.String(), "TEST_ONLY_NOT_SIGNED") || strings.Contains(w.Body.String(), "QA_PROXY_PASSWORD") || strings.Contains(w.Body.String(), "backend-api/") || strings.Contains(w.Body.String(), "credentials_enc") {
			t.Fatal("internal data exposed")
		}
	}
	var count int
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM ledger").Scan(&count)
	if count != 0 {
		t.Fatal("link generation charged money")
	}
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM proxy_leases").Scan(&count)
	if count != 0 {
		t.Fatal("proxy capacity not released")
	}
	// Known preflight failures wait for the configured interval.
	h.resetRates()
	next := expect(t, h.request("POST", "/api/orders", prepareRequest(), h.member, map[string]string{"Idempotency-Key": uid()}), 200)
	job, e = h.a.claim(ctx, "checkout")
	if e != nil || job == nil {
		t.Fatal("next claim")
	}
	if e = h.a.finishCheckout(ctx, job, checkoutOutput{}, checkoutFailure("network", true, false)); e != nil {
		t.Fatal(e)
	}
	var when time.Time
	h.a.DB.QueryRow(ctx, "SELECT next_at,state FROM jobs WHERE order_id=$1", next["id"]).Scan(&when, &code)
	if code != "queued" || time.Until(when) < 25*time.Second {
		t.Fatal("retry delay ignored")
	}
	job, e = h.a.claim(ctx, "checkout")
	if e != nil || job != nil {
		t.Fatal("retry ran early")
	}
	expect(t, h.request("POST", "/api/orders/"+next["id"].(string)+"/cancel", M{}, h.member, nil), 200)
	h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM checkout_runs WHERE order_id=$1", next["id"]).Scan(&credentials)
	if credentials != "" {
		t.Fatal("cancelled credential retained")
	}
	// Expiry must remain visible after maintenance erases the encrypted URL.
	if _, e = h.a.DB.Exec(ctx, "UPDATE checkout_runs SET hosted_url_enc='',link_expires_at=now()-interval '1 minute' WHERE order_id=$1", id); e != nil {
		t.Fatal(e)
	}
	if h.a.checkoutDetails(ctx, id)["expired"] != true {
		t.Fatal("erased expired link still looks pending")
	}
}

func TestProxySubscriptionRegionRouting(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	ph, backup, us := h.addProxy("PH", 1), h.addProxy("PH", 1), h.addProxy("US", 1)
	settings := M{"default_id": ph, "match_country": true, "request_timeout_seconds": 60}
	expect(t, h.request("PUT", "/api/admin/settings/proxy", settings, h.admin, nil), 200)
	acquire := func(country, wantID string) *paymentProxy {
		t.Helper()
		tx, e := h.a.DB.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		p, e := h.a.acquireProxyTx(ctx, tx, country, time.Minute)
		if wantID == "" {
			if !errors.Is(e, ErrProxyUnavailable) {
				t.Fatalf("expected no eligible %s proxy, got %v", country, e)
			}
			return nil
		}
		if e != nil || p.ID != wantID {
			t.Fatalf("wrong proxy selected for %s: %v, %v", country, p, e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return p
	}
	first := acquire("PH", ph)
	second := acquire("PH", backup) // Full preferred proxy uses the same-region pool.
	acquire("PH", "")               // The free US proxy must never fill PH capacity.
	third := acquire("US", us)      // PH preference must not pin US orders to PH.
	acquire("JP", "")
	update := M{"name": "Region test", "country": "US", "enabled": true, "weight": 1, "capacity": 1}
	expect(t, h.request("PUT", "/api/admin/resources/"+ph, update, h.admin, nil), 409)
	for _, lease := range []*paymentProxy{first, second, third} {
		h.a.releaseProxy(ctx, lease.Lease)
	}
	// Region edits preserve encrypted credentials when the address is left blank.
	update["country"] = " us "
	expect(t, h.request("PUT", "/api/admin/resources/"+ph, update, h.admin, nil), 200)
	preferredUS := acquire("US", ph)
	if preferredUS.Endpoint != "http://user:QA_PROXY_PASSWORD@proxy.example.com:8080" {
		t.Fatal("region edit lost proxy credentials")
	}
	h.a.releaseProxy(ctx, preferredUS.Lease)
	update["country"], update["enabled"] = "US", false
	expect(t, h.request("PUT", "/api/admin/resources/"+ph, update, h.admin, nil), 200)
	alternativeUS := acquire("US", us)
	h.a.releaseProxy(ctx, alternativeUS.Lease)
	update["country"] = "ZZ"
	expect(t, h.request("PUT", "/api/admin/resources/"+ph, update, h.admin, nil), 400)
	settings["match_country"] = false
	expect(t, h.request("PUT", "/api/admin/settings/proxy", settings, h.admin, nil), 400)
	// Even legacy configuration cannot bypass the country boundary.
	if _, e := h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{match_country}','false') WHERE key='proxy'`); e != nil {
		t.Fatal(e)
	}
	acquire("JP", "")
	if e := h.a.seed(ctx); e != nil || h.a.config(ctx, "proxy")["match_country"] != true {
		t.Fatalf("legacy proxy settings not migrated: %v", e)
	}
}

func TestCheckoutInterruptedWriteReleasesProxy(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	h.addProxy("PH", 1)
	order := expect(t, h.request("POST", "/api/orders", prepareRequest(), h.member, map[string]string{"Idempotency-Key": uid()}), 200)
	j, e := h.a.claim(ctx, "checkout")
	if e != nil || j == nil {
		t.Fatalf("claim: %v", e)
	}
	if e = h.a.BeforeSideEffect(ctx, j); e != nil {
		t.Fatal(e)
	}
	if _, e = h.a.DB.Exec(ctx, "UPDATE jobs SET lease_until=now()-interval '1 second' WHERE id=$1", j.ID); e != nil {
		t.Fatal(e)
	}
	claimed, e := h.a.claim(ctx, "checkout")
	if e != nil || claimed != nil {
		t.Fatalf("uncertain checkout retried: %v", e)
	}
	var status, credentials string
	var leases int
	if e = h.a.DB.QueryRow(ctx, "SELECT o.status,c.credentials_enc FROM orders o JOIN checkout_runs c ON c.order_id=o.id WHERE o.id=$1", order["id"]).Scan(&status, &credentials); e != nil {
		t.Fatal(e)
	}
	if e = h.a.DB.QueryRow(ctx, "SELECT count(*) FROM proxy_leases").Scan(&leases); e != nil {
		t.Fatal(e)
	}
	if status != "review" || credentials != "" || leases != 0 {
		t.Fatal("interrupted job retained credentials or occupied proxy")
	}
	if e = h.a.finishCheckout(ctx, j, checkoutOutput{URL: "https://pay.openai.com/c/pay/cs_test_stale"}, nil); !errors.Is(e, ErrLeaseLost) {
		t.Fatal("stale worker overwrote recovered result")
	}
}

func TestProxyCapacityDefaultsAndBilling(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	id := h.addProxy("PH", 1)
	configs := M{"default_id": id, "match_country": true, "request_timeout_seconds": 60}
	expect(t, h.request("PUT", "/api/admin/settings/proxy", configs, h.member, nil), 403)
	expect(t, h.request("PUT", "/api/admin/settings/proxy", configs, h.admin, nil), 200)
	leases := make(chan *paymentProxy, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, e := h.a.DB.Begin(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			defer tx.Rollback(ctx)
			p, e := h.a.acquireProxyTx(ctx, tx, "PH", time.Minute)
			if errors.Is(e, ErrProxyUnavailable) {
				return
			}
			if e != nil {
				t.Error(e)
				return
			}
			if e = tx.Commit(ctx); e != nil {
				t.Error(e)
				return
			}
			leases <- p
		}()
	}
	wg.Wait()
	close(leases)
	got := 0
	for lease := range leases {
		got++
		h.a.releaseProxy(ctx, lease.Lease)
	}
	if got != 1 {
		t.Fatalf("capacity admitted %d tasks", got)
	}
	tx, e := h.a.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	_, e = h.a.acquireProxyTx(ctx, tx, "US", time.Minute)
	tx.Rollback(ctx)
	if !errors.Is(e, ErrProxyUnavailable) {
		t.Fatal("wrong region selected")
	}
	expect(t, h.request("DELETE", "/api/admin/resources/"+id, nil, h.admin, nil), 200)
	if str(h.a.config(ctx, "proxy"), "default_id") != "" {
		t.Fatal("deleted default still selected")
	}
	if _, err := h.pickBilling(); err == nil {
		t.Fatal("missing billing must not generate an address")
	}
	for _, path := range []string{"/api/billing/prepare", "/api/v1/billing/prepare"} {
		expect(t, h.request("POST", path, M{}, h.member, nil), 404)
	}
}
