package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func subscriptionFixture(t *testing.T, keep bool) (*harness, string, *fakePayment) {
	h := setupTest(t)
	h.saveTestBilling("subscription@example.com")
	ctx := context.Background()
	if _, e := h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID); e != nil {
		t.Fatal(e)
	}
	p := &fakePayment{events: make(chan paymentEvent, 16)}
	h.a.paymentStart = func(_ context.Context, in paymentInput) (paymentProcess, error) {
		if !in.AutoConfirm || in.MaxAmount != 2000 || in.Currency != "php" || !validBilling(in.Billing) || in.Billing["source"] != "saved" {
			t.Error("automatic input or billing invalid")
		}
		return p, nil
	}
	t.Cleanup(func() {
		h.a.paymentMu.Lock()
		for _, s := range h.a.payments {
			s.cancel()
		}
		h.a.paymentMu.Unlock()
		time.Sleep(80 * time.Millisecond)
	})
	body := prepareRequest()
	body["workflow"] = "subscription"
	body["card"] = paymentPrepareBody()["card"]
	body["keep_renewal"] = keep
	headers := map[string]string{"Idempotency-Key": uid()}
	v := expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200)
	id := str(v, "id")
	replay := expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200)
	if replay["replayed"] != true || replay["id"] != id {
		t.Fatal("subscription replay duplicated")
	}
	expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 409)
	h.a.DB.Exec(ctx, "UPDATE subscription_runs SET max_amount=2000 WHERE order_id=$1", id)
	// These existing tests exercise historical Chromium orders after migration.
	h.a.paymentMu.Lock()
	queued := h.a.payments[id]
	card, billing := queued.protocol.card, queued.protocol.billing
	close(queued.protocol.done)
	h.a.paymentMu.Unlock()
	if _, err := h.a.DB.Exec(ctx, "DELETE FROM subscription_protocol WHERE order_id=$1", id); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	raw, err := h.a.decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var legacy checkoutInput
	if err = json.Unmarshal([]byte(raw), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.SessionToken, legacy.Cookies = "", ""
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err = h.a.encrypt(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.a.DB.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2 WHERE order_id=$1", id, encrypted); err != nil {
		t.Fatal(err)
	}
	if _, err = h.a.DB.Exec(ctx, "UPDATE checkout_runs SET credentials_enc=$2 WHERE order_id=$1", id, encrypted); err != nil {
		t.Fatal(err)
	}
	h.a.launchSubscription(id, queued.attempt, card, billing)
	paymentBalances(t, h, 90000, 10000)
	return h, id, p
}

func completeSubscriptionCheckout(t *testing.T, h *harness, id string) {
	h.addProxy("PH", 2)
	job, e := h.a.claim(context.Background(), "checkout")
	if e != nil || job == nil || job.OrderID != id {
		t.Fatal("checkout not claimed", e)
	}
	if e = h.a.recordOrderProxyIP(context.Background(), job, "203.0.113.2"); e != nil {
		t.Fatal(e)
	}
	if e = h.a.finishCheckout(context.Background(), job, checkoutOutput{URL: "https://pay.openai.com/c/pay/cs_test_SubscriptionPrivate"}, nil); e != nil {
		t.Fatal(e)
	}
}

func assertSubscriptionPrivate(t *testing.T, h *harness, id string) {
	for _, q := range []struct {
		path   string
		cookie *http.Cookie
	}{{"/api/orders/" + id, h.member}, {"/api/admin/orders/" + id, h.admin}} {
		w := h.request("GET", q.path, nil, q.cookie, nil)
		expect(t, w, 200)
		if q.cookie != h.admin && (strings.Contains(w.Body.String(), "cs_test_SubscriptionPrivate") || strings.Contains(w.Body.String(), "pay.openai.com") || strings.Contains(w.Body.String(), "checkout_links")) {
			t.Fatal("customer received administrator checkout link")
		}
		for _, secret := range []string{"backend-api/", "TEST_ONLY_NOT_SIGNED", "QA_PROXY_PASSWORD", "credentials_enc", "4242424242424242", "payment_pages", "details_enc", "profile_id", "100 Test Street"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("subscription response leaked", secret)
			}
		}
	}
	expect(t, h.request("GET", "/api/orders/"+id, nil, h.other, nil), 404)
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.member, nil), 403)
}

func TestSubscriptionFullFlowAndRenewalChoice(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable", true: "keep"}[keep], func(t *testing.T) {
			h, id, p := subscriptionFixture(t, keep)
			completeSubscriptionCheckout(t, h, id)
			p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
			waitPayment(t, h, id, "submitting")
			until := time.Now().Add(time.Second)
			for p.commits.Load() != 1 && time.Now().Before(until) {
				time.Sleep(10 * time.Millisecond)
			}
			if p.commits.Load() != 1 {
				t.Fatal("automatic confirmation not exactly once")
			}
			p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
			waitPayment(t, h, id, "succeeded")
			var status, renewal string
			h.a.DB.QueryRow(context.Background(), "SELECT status,renewal FROM orders WHERE id=$1", id).Scan(&status, &renewal)
			if keep {
				if status != "processing" || renewal != "kept" {
					t.Fatal("keep must still verify activation", status, renewal)
				}
				h.a.renewalClient = func() *http.Client {
					return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
						if r.Method != "GET" {
							t.Fatal("keep must never cancel")
						}
						return providerResponse(200, M{"is_active": true, "will_renew": true, "plan_type": "premium"}), nil
					})}
				}
				next, lease, e := h.a.claimRenewal(context.Background())
				if e != nil || next != id {
					t.Fatal("activation task missing", e)
				}
				h.a.runRenewal(context.Background(), id, lease)
				h.a.DB.QueryRow(context.Background(), "SELECT status,renewal FROM orders WHERE id=$1", id).Scan(&status, &renewal)
				if status != "succeeded" || renewal != "kept" {
					t.Fatal("keep activation did not finish", status, renewal)
				}
			} else {
				if status != "processing" {
					t.Fatal("order completed before renewal")
				}
				writes := 0
				reads := 0
				h.a.renewalClient = func() *http.Client {
					return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
						if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("Chatgpt-Account-Id") != "test-space" {
							t.Error("missing renewal authentication")
						}
						body := `{"returned_amount":null}`
						if r.Method == "POST" {
							writes++
							raw, _ := io.ReadAll(r.Body)
							var v M
							json.Unmarshal(raw, &v)
							if len(v) != 1 || v["account_id"] != "test-space" {
								t.Error("incorrect cancel payload")
							}
						} else {
							reads++
							if r.URL.Query().Get("account_id") != "test-space" {
								t.Error("missing account query")
							}
							if writes == 0 {
								body = `{"id":"subscription-id","is_active":true,"plan_type":"premium","will_renew":true}`
							} else {
								body = `{"id":"subscription-id","is_active":true,"plan_type":"premium","will_renew":false}`
							}
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
					})}
				}
				next, lease, e := h.a.claimRenewal(context.Background())
				if e != nil || next != id {
					t.Fatal("missing renewal job", e)
				}
				h.a.runRenewal(context.Background(), next, lease)
				h.a.DB.QueryRow(context.Background(), "SELECT status,renewal FROM orders WHERE id=$1", id).Scan(&status, &renewal)
				if status != "succeeded" || renewal != "disabled" || writes != 1 || reads != 2 {
					t.Fatal("renewal not verified", status, renewal, writes, reads)
				}
			}
			paymentBalances(t, h, 90000, 0)
			assertSubscriptionPrivate(t, h, id)
			var enc string
			h.a.DB.QueryRow(context.Background(), "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1", id).Scan(&enc)
			if enc != "" {
				t.Fatal("finished credentials retained")
			}
		})
	}
}

func TestSubscriptionCapAndQueueTimeout(t *testing.T) {
	for _, mode := range []string{"cap", "currency", "crash"} {
		t.Run(mode, func(t *testing.T) {
			h, id, p := subscriptionFixture(t, false)
			if mode == "crash" {
				h.a.DB.Exec(context.Background(), "UPDATE payment_runs SET expires_at=now()-interval '1 second' WHERE order_id=$1", id)
				if e := h.a.RecoverPayments(context.Background()); e != nil {
					t.Fatal(e)
				}
				waitPayment(t, h, id, "failed")
			} else {
				completeSubscriptionCheckout(t, h, id)
				currency := "php"
				amount := int64(2001)
				if mode == "currency" {
					currency = "usd"
					amount = 1000
				}
				p.events <- paymentEvent{Type: "quote", Amount: amount, Currency: currency}
				waitPayment(t, h, id, "failed")
			}
			if p.commits.Load() != 0 {
				t.Fatal("unexpected charge")
			}
			paymentBalances(t, h, 100000, 0)
			assertSubscriptionPrivate(t, h, id)
		})
	}
}

func TestRenewalUnknownWriteNeverReplayed(t *testing.T) {
	h, id, p := subscriptionFixture(t, false)
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "succeeded")
	writes := 0
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method == "POST" {
				writes++
				return nil, errors.New("PRIVATE_UPSTREAM_ERROR")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"is_active":true,"plan_type":"premium","will_renew":true}`)), Header: http.Header{}}, nil
		})}
	}
	for i := 0; i < 2; i++ {
		h.a.DB.Exec(context.Background(), "UPDATE subscription_runs SET next_at=now() WHERE order_id=$1", id)
		next, lease, e := h.a.claimRenewal(context.Background())
		if e != nil || next != id {
			t.Fatal(e)
		}
		h.a.runRenewal(context.Background(), next, lease)
	}
	if writes != 1 {
		t.Fatal("uncertain cancellation was replayed")
	}
	var status string
	h.a.DB.QueryRow(context.Background(), "SELECT status FROM orders WHERE id=$1", id).Scan(&status)
	if status != "processing" {
		t.Fatal("unconfirmed cancellation counted as complete")
	}
	assertSubscriptionPrivate(t, h, id)
}
