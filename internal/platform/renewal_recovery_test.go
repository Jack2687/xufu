package platform

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func paidRenewalFailure(t *testing.T, started bool) (*harness, string) {
	t.Helper()
	h, id, p := subscriptionFixture(t, false)
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "succeeded")
	ctx := context.Background()
	if _, err := h.a.DB.Exec(ctx, "UPDATE subscription_runs SET cancel_started=$2 WHERE order_id=$1", id, started); err != nil {
		t.Fatal(err)
	}
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_expired"}}`))}, nil
		})}
	}
	got, lease, err := h.a.claimRenewal(ctx)
	if err != nil || got != id {
		t.Fatal("claim", err)
	}
	h.a.runRenewal(ctx, id, lease)
	var status, renewal string
	if err = h.a.DB.QueryRow(ctx, "SELECT status,renewal FROM orders WHERE id=$1", id).Scan(&status, &renewal); err != nil || status != "succeeded" || renewal != "review" {
		t.Fatal("paid partial failure", status, renewal, err)
	}
	h.resetRates()
	return h, id
}

func TestRenewalRecoveryPreservesPaymentAndWriteFence(t *testing.T) {
	for _, mode := range []string{"not_sent", "uncertain_write", "legacy_not_sent"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			started := mode == "uncertain_write"
			h, order := paidRenewalFailure(t, started)
			if mode == "legacy_not_sent" {
				h.a.DB.Exec(ctx, "UPDATE subscription_runs SET account_hash='' WHERE order_id=$1", order)
			}
			var before int
			h.a.DB.QueryRow(ctx, "SELECT count(*) FROM ledger").Scan(&before)
			id := newStandalone(t, h, "subscription@example.com")
			var taskFence bool
			if err := h.a.DB.QueryRow(ctx, "SELECT cancel_started FROM renewal_tasks WHERE id=$1", id).Scan(&taskFence); err != nil || taskFence != started {
				t.Fatal("lost write fence", err)
			}
			if got, _, err := h.a.claimRenewal(ctx); err != nil || got != "" {
				t.Fatal("old worker reclaimed", err)
			}
			body := prepareRequest()
			body["workflow"] = "subscription"
			body["card"] = paymentPrepareBody()["card"]
			expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 409)
			writes, closed := 0, false
			h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{renewal_check_limit}','1') WHERE key='payment'`)
			h.a.renewalClient = func() *http.Client {
				return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == "POST" {
						writes++
						closed = true
					}
					body := `{"is_active":true,"plan_type":"premium","will_renew":true}`
					if closed {
						body = `{"is_active":true,"plan_type":"premium","will_renew":false}`
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
			}
			runStandalone(t, h, id)
			if started {
				standaloneStatus(t, h, id, "review")
				if writes != 0 {
					t.Fatal("uncertain write replayed")
				}
				if next := newStandalone(t, h, "subscription@example.com"); next != id {
					t.Fatal("continuation replaced")
				}
				closed = true
				runStandalone(t, h, id)
			}
			standaloneStatus(t, h, id, "disabled")
			if (!started && writes != 1) || (started && writes != 0) {
				t.Fatal("unexpected cancellation count", writes)
			}
			var status, renewal, payment, fee string
			var after int
			err := h.a.DB.QueryRow(ctx, `SELECT o.status,o.renewal,p.state,p.fee_state FROM orders o JOIN payment_runs p ON p.order_id=o.id WHERE o.id=$1`, order).Scan(&status, &renewal, &payment, &fee)
			if err != nil || status != "succeeded" || renewal != "disabled" || payment != "succeeded" || fee != "charged" {
				t.Fatal("recovery outcome", status, renewal, payment, fee, err)
			}
			h.a.DB.QueryRow(ctx, "SELECT count(*) FROM ledger").Scan(&after)
			if after != before {
				t.Fatal("recovery changed financial ledger")
			}
			paymentBalances(t, h, 90000, 0)
			assertSubscriptionPrivate(t, h, order)
			for _, path := range []string{"/api/renewals", "/api/renewals/" + id} {
				r := h.request("GET", path, nil, h.member, nil)
				for _, private := range []string{"account_hash", "renewal_task_id", "cancel_started", "test-space", "Bearer", "backend-api", "credentials_enc"} {
					if strings.Contains(r.Body.String(), private) {
						t.Fatal("private field leaked", private)
					}
				}
			}
		})
	}
}

func TestRenewalRecoveryAdmissionAndMigration(t *testing.T) {
	for _, mode := range []string{"active", "payment_unknown", "wrong_owner", "wrong_workspace", "live_lease", "legacy_uncertain"} {
		t.Run(mode, func(t *testing.T) {
			h, order := paidRenewalFailure(t, false)
			ctx := context.Background()
			cookie := h.member
			switch mode {
			case "active":
				h.a.DB.Exec(ctx, "UPDATE subscription_runs SET renewal_state='pending' WHERE order_id=$1", order)
			case "payment_unknown":
				h.a.DB.Exec(ctx, "UPDATE payment_runs SET state='review' WHERE order_id=$1", order)
			case "wrong_owner":
				cookie = h.other
			case "wrong_workspace":
				h.a.DB.Exec(ctx, "UPDATE subscription_runs SET account_hash=$2 WHERE order_id=$1", order, hash("other-workspace"))
			case "live_lease":
				h.a.DB.Exec(ctx, "UPDATE subscription_runs SET lease_token='busy',lease_until=now()+interval '1 minute' WHERE order_id=$1", order)
			case "legacy_uncertain":
				h.a.DB.Exec(ctx, "UPDATE subscription_runs SET account_hash='',cancel_started=true WHERE order_id=$1", order)
			}
			expect(t, h.request("POST", "/api/renewals", M{"session": testSession("subscription@example.com"), "accepted": true}, cookie, map[string]string{"Idempotency-Key": uid()}), 409)
			var n int
			h.a.DB.QueryRow(ctx, "SELECT count(*) FROM renewal_tasks").Scan(&n)
			if n != 0 {
				t.Fatal("blocked request created work")
			}
		})
	}
	t.Run("migration", func(t *testing.T) {
		h, id := paidRenewalFailure(t, false)
		ctx := context.Background()
		h.a.DB.Exec(ctx, "UPDATE orders SET status='review' WHERE id=$1", id)
		for i := 0; i < 2; i++ {
			if _, err := h.a.DB.Exec(ctx, schema); err != nil {
				t.Fatal(err)
			}
		}
		var status string
		var events int
		h.a.DB.QueryRow(ctx, "SELECT status FROM orders WHERE id=$1", id).Scan(&status)
		h.a.DB.QueryRow(ctx, "SELECT count(*) FROM order_events WHERE order_id=$1 AND message='订阅成功，取消自动续费失败。请在续费管理粘贴该账号的新 Session 继续处理，无需重新付款。'", id).Scan(&events)
		if status != "succeeded" || events != 1 {
			t.Fatal("migration not idempotent", status, events)
		}
		h.a.DB.Exec(ctx, "UPDATE orders SET status='review' WHERE id=$1", id)
		h.a.DB.Exec(ctx, "UPDATE payment_runs SET state='review' WHERE order_id=$1", id)
		if _, err := h.a.DB.Exec(ctx, schema); err != nil {
			t.Fatal(err)
		}
		h.a.DB.QueryRow(ctx, "SELECT status FROM orders WHERE id=$1", id).Scan(&status)
		if status != "review" {
			t.Fatal("migration falsely completed unknown payment")
		}
	})
}

func TestRenewalRecoveryConcurrentAdmission(t *testing.T) {
	h, order := paidRenewalFailure(t, false)
	ctx := context.Background()
	h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{order_per_minute}','100') WHERE key='risk'`)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := h.request("POST", "/api/renewals", M{"session": testSession("subscription@example.com"), "accepted": true}, h.member, map[string]string{"Idempotency-Key": uid()})
			if r.Code != 200 {
				t.Error("admission failed", r.Code)
				return
			}
			ids <- str(expect(t, r, 200), "id")
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for next := range ids {
		if id != "" && id != next {
			t.Fatal("duplicate continuation")
		}
		id = next
	}
	var tasks int
	var link, state string
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM renewal_tasks").Scan(&tasks)
	h.a.DB.QueryRow(ctx, "SELECT renewal_task_id,renewal_state FROM subscription_runs WHERE order_id=$1", order).Scan(&link, &state)
	if tasks != 1 || link != id || state != "delegated" {
		t.Fatal("incorrect handoff", tasks, state)
	}
}
