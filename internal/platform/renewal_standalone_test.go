package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newStandalone(t *testing.T, h *harness, email string) string {
	t.Helper()
	v := expect(t, h.request("POST", "/api/renewals", M{"session": testSession(email), "accepted": true}, h.member, map[string]string{"Idempotency-Key": uid()}), 200)
	return str(v, "id")
}
func runStandalone(t *testing.T, h *harness, id string) {
	t.Helper()
	got, lease, e := h.a.claimStandaloneRenewal(context.Background())
	if e != nil || got != id {
		t.Fatalf("claim: id=%s expected=%s error=%v", got, id, e)
	}
	h.a.runStandaloneRenewal(context.Background(), got, lease)
}
func standaloneStatus(t *testing.T, h *harness, id string, want string) M {
	t.Helper()
	v := expect(t, h.request("GET", "/api/renewals/"+id, nil, h.member, nil), 200)
	task := v["task"].(map[string]any)
	if task["status"] != want {
		t.Fatalf("expected %s got %v", want, task["status"])
	}
	return task
}
func TestStandaloneRenewalOutcomes(t *testing.T) {
	for _, mode := range []string{"close", "already_off", "no_subscription", "unknown_write", "read_error", "expired", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			h := setupTest(t)
			ctx := context.Background()
			h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{renewal_check_limit}','2') WHERE key='payment'`)
			id := newStandalone(t, h, "renewal@example.com")
			writes, reads := 0, 0
			h.a.renewalClient = func() *http.Client {
				return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("Chatgpt-Account-Id") != "test-space" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
						t.Error("missing account authorization")
					}
					if r.Header.Get("Proxy-Authorization") != "" {
						t.Error("proxy credentials leaked")
					}
					body := `{"id":"subscription-123","is_active":true,"will_renew":true}`
					if r.Method == "POST" {
						writes++
						if r.URL.String() != subscriptionEndpoint+"/cancel" {
							t.Error("wrong cancellation endpoint")
						}
						b, _ := io.ReadAll(r.Body)
						var payload M
						if json.Unmarshal(b, &payload) != nil || len(payload) != 1 || payload["account_id"] != "test-space" {
							t.Error("wrong cancellation payload")
						}
						if mode == "unknown_write" {
							return nil, errors.New("PRIVATE_PROVIDER_ERROR_TEST_ONLY")
						}
						body = `{"success":true}`
					} else {
						reads++
						if r.URL.Query().Get("account_id") != "test-space" {
							t.Error("missing workspace query")
						}
						switch mode {
						case "close":
							if writes > 0 {
								body = `{"is_active":true,"will_renew":false}`
							}
						case "already_off":
							body = `{"is_active":true,"will_renew":false}`
						case "no_subscription":
							body = `{"is_active":false,"will_renew":false}`
						case "read_error":
							return nil, errors.New("PRIVATE_PROVIDER_ERROR_TEST_ONLY")
						case "mismatch":
							body = `{"account_id":"wrong-space","is_active":true,"will_renew":false}`
						}
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				})}
			}
			if mode == "expired" {
				h.a.DB.Exec(ctx, "UPDATE renewal_tasks SET credentials_expires_at=now()-interval '1 second' WHERE id=$1", id)
			}
			runStandalone(t, h, id)
			want := "disabled"
			switch mode {
			case "no_subscription":
				want = "no_subscription"
			case "unknown_write", "read_error", "mismatch":
				standaloneStatus(t, h, id, "checking")
				h.a.DB.Exec(ctx, "UPDATE renewal_tasks SET next_at=now() WHERE id=$1", id)
				runStandalone(t, h, id)
				want = "review"
			case "expired":
				want = "review"
			}
			standaloneStatus(t, h, id, want)
			if (mode == "close" || mode == "unknown_write") && writes != 1 {
				t.Fatal("cancellation must be at most once", writes)
			}
			if mode != "close" && mode != "unknown_write" && writes != 0 {
				t.Fatal("unnecessary cancellation", writes)
			}
			if mode == "expired" && reads != 0 {
				t.Fatal("expired authorization was used")
			}
			var enc string
			var financial int
			h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM renewal_tasks WHERE id=$1", id).Scan(&enc)
			if enc != "" {
				t.Fatal("finished credentials not cleared")
			}
			h.a.DB.QueryRow(ctx, "SELECT (SELECT count(*) FROM ledger)+(SELECT count(*) FROM orders)+(SELECT count(*) FROM payment_runs)").Scan(&financial)
			if financial != 0 {
				t.Fatal("standalone renewal created financial activity")
			}
			// Fresh session after an uncertain write resumes reads, never a second cancellation.
			if mode == "unknown_write" {
				if again := newStandalone(t, h, "renewal@example.com"); again != id {
					t.Fatal("uncertain request replaced")
				}
				runStandalone(t, h, id)
				if writes != 1 {
					t.Fatal("resubmission replayed cancellation")
				}
			}
			for _, path := range []string{"/api/renewals", "/api/renewals/" + id} {
				result := h.request("GET", path, nil, h.member, nil)
				for _, secret := range []string{"backend-api", "chatgpt.com", "Bearer", "TEST_ONLY_NOT_SIGNED", "PRIVATE_PROVIDER", "credentials_enc", "account_hash", "lease_token", "accessToken", "test-space"} {
					if strings.Contains(result.Body.String(), secret) {
						t.Fatal("private response leaked", secret)
					}
				}
			}
		})
	}
}

func TestStandaloneRenewalIdempotencyOwnershipAndAdmission(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	body := M{"session": testSession("renewal@example.com"), "accepted": true}
	headers := map[string]string{"Idempotency-Key": uid()}
	expect(t, h.request("POST", "/api/renewals", body, nil, headers), 401)
	expect(t, h.request("POST", "/api/renewals", M{"session": body["session"], "accepted": false}, h.member, headers), 400)
	expect(t, h.request("POST", "/api/renewals", M{"session": M{}, "accepted": true}, h.member, headers), 400)
	h.resetRates()
	id := str(expect(t, h.request("POST", "/api/renewals", body, h.member, headers), 200), "id")
	for i := 0; i < 3; i++ {
		if got := str(expect(t, h.request("POST", "/api/renewals", body, h.member, headers), 200), "id"); got != id {
			t.Fatal("idempotency duplicated task")
		}
	}
	expect(t, h.request("POST", "/api/renewals", body, h.member, headers), 200)
	expect(t, h.request("POST", "/api/renewals", body, h.member, headers), 429)
	h.resetRates()
	if got := newStandalone(t, h, "renewal@example.com"); got != id {
		t.Fatal("different key duplicated active task")
	}
	expect(t, h.request("GET", "/api/renewals/"+id, nil, h.other, nil), 404)
	expect(t, h.request("GET", "/api/renewals/"+id, nil, nil, nil), 401)
	if rows := expect(t, h.request("GET", "/api/renewals", nil, h.other, nil), 200)["items"].([]any); len(rows) != 0 {
		t.Fatal("other user saw task")
	}
	expect(t, h.request("POST", "/api/renewals", body, h.other, map[string]string{"Idempotency-Key": uid()}), 409)
	expect(t, h.request("POST", "/api/renewals", body, h.member, map[string]string{"Idempotency-Key": uid(), "Origin": "https://evil.example"}), 403)
	var enc string
	h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM renewal_tasks WHERE id=$1", id).Scan(&enc)
	if enc == "" || strings.Contains(enc, "TEST_ONLY_NOT_SIGNED") {
		t.Fatal("authorization not encrypted")
	}
	// Subscription admission uses the same lock and checks pending cancellation before billing.
	h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
		return &fakePayment{events: make(chan paymentEvent, 16)}, nil
	}
	expect(t, h.request("POST", "/api/orders", M{"product_id": "pro", "session": testSession("renewal@example.com"), "country": "PH", "seats": 1, "accepted": true, "workflow": "subscription", "card": paymentPrepareBody()["card"]}, h.member, map[string]string{"Idempotency-Key": uid()}), 409)
}

func TestStandaloneRenewalConcurrentClaimsAndRestart(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{order_per_minute}','20') WHERE key='risk'`)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ids <- newStandalone(t, h, "renewal@example.com") }()
	}
	wg.Wait()
	close(ids)
	id := ""
	for x := range ids {
		if id != "" && id != x {
			t.Fatal("concurrent duplicate tasks")
		}
		id = x
	}
	var claims atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, _, e := h.a.claimStandaloneRenewal(ctx)
			if e != nil {
				t.Error(e)
			}
			if x != "" {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("multiple workers claimed one task", claims.Load())
	}
	// Simulate a process dying after persisting the write fence. Recovery must only read.
	h.a.DB.Exec(ctx, "UPDATE renewal_tasks SET cancel_started=true,lease_until=now()-interval '1 second' WHERE id=$1", id)
	writes := 0
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method == "POST" {
				writes++
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"is_active":true,"will_renew":false}`))}, nil
		})}
	}
	runStandalone(t, h, id)
	standaloneStatus(t, h, id, "disabled")
	if writes != 0 {
		t.Fatal("restart replayed cancellation")
	}
}

func TestStandaloneRenewalSharedCapacityAndStaleLease(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	id := newStandalone(t, h, "renewal@example.com")
	h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{max_parallel}','1') WHERE key='payment'`)
	got, lease, e := h.a.claimStandaloneRenewal(ctx)
	if e != nil || got != id {
		t.Fatal("claim", e)
	}
	// With this capacity occupied, the existing subscription renewal queue cannot start work.
	if next, _, e := h.a.claimRenewal(ctx); e != nil || next != "" {
		t.Fatal("capacity not shared", e)
	}
	h.a.DB.Exec(ctx, "UPDATE renewal_tasks SET lease_token='replacement',lease_until=now()+interval '110 seconds' WHERE id=$1", id)
	calls := 0
	h.a.renewalClient = func() *http.Client { calls++; return &http.Client{} }
	h.a.runStandaloneRenewal(ctx, id, lease)
	if calls != 0 {
		t.Fatal("stale worker contacted provider")
	}
	standaloneStatus(t, h, id, "processing")
	h.a.DB.Exec(ctx, "UPDATE renewal_tasks SET credentials_expires_at=$2 WHERE id=$1", id, time.Now().Add(-time.Hour))
	if e = h.a.Maintain(ctx); e != nil {
		t.Fatal(e)
	}
	var enc string
	h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM renewal_tasks WHERE id=$1", id).Scan(&enc)
	if enc != "" {
		t.Fatal("expired credentials retained")
	}
}

func TestStandaloneRenewalCannotRaceSubscription(t *testing.T) {
	h, id, p := subscriptionFixture(t, false)
	ctx := context.Background()
	expect(t, h.request("POST", "/api/renewals", M{"session": testSession("subscription@example.com"), "accepted": true}, h.member, map[string]string{"Idempotency-Key": uid()}), 409)
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "succeeded")
	h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{max_parallel}','1') WHERE key='payment'`)
	next, _, e := h.a.claimRenewal(ctx)
	if e != nil || next != id {
		t.Fatal("automatic cancellation not claimed", e)
	}
	// A second account is queued but cannot consume already occupied renewal capacity.
	second := testSession("another@example.com")
	claims, _ := json.Marshal(M{"exp": time.Now().Add(time.Hour).Unix()})
	second["account"] = M{"id": "second-account", "planType": "plus"}
	second["accessToken"] = "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".TEST_ONLY_NOT_SIGNED"
	body := M{"session": second, "accepted": true}
	headers := map[string]string{"Idempotency-Key": uid()}
	task := expect(t, h.request("POST", "/api/renewals", body, h.member, headers), 200)
	if next, _, e = h.a.claimStandaloneRenewal(ctx); e != nil || next != "" {
		t.Fatal("shared capacity exceeded", e)
	}
	expect(t, h.request("POST", "/api/renewals", M{"session": testSession("third@example.com"), "accepted": true}, h.member, headers), 409)
	h.a.DB.Exec(ctx, "UPDATE subscription_runs SET lease_token=NULL,lease_until=NULL WHERE order_id=$1", id)
	if next, _, e = h.a.claimStandaloneRenewal(ctx); e != nil || next != str(task, "id") {
		t.Fatal("queued cancellation not admitted after capacity release", e)
	}
}
