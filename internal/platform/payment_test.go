package platform

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePayment struct {
	events  chan paymentEvent
	commits atomic.Int32
}

func (p *fakePayment) Events() <-chan paymentEvent { return p.events }
func (p *fakePayment) Send(c paymentCommand) error {
	if c.Type == "confirm" {
		p.commits.Add(1)
	}
	return nil
}
func (p *fakePayment) Close() {}
func paymentFixture(t *testing.T) (*harness, string, *fakePayment) {
	h := setupTest(t)
	h.saveTestBilling("payer@example.com")
	ctx := context.Background()
	id := uid()
	proxy, _ := h.a.encrypt("http://user:PRIVATE_PROXY@proxy.example.com:8080")
	link, _ := h.a.encrypt("https://pay.openai.com/c/pay/cs_test_QA123")
	for _, q := range []struct {
		s    string
		args []any
	}{
		{"UPDATE users SET balance=10000 WHERE id=$1", []any{h.memberID}},
		{"INSERT INTO resources(id,name,country,endpoint_enc,capacity) VALUES('payment_proxy','Test','PH',$1,2)", []any{proxy}},
		{"INSERT INTO orders(id,user_id,product_id,product_name,email,country,status,fee,idempotency_key,request_hash) VALUES($1,$2,'pro','Pro','payer@example.com','PH','awaiting_payment',100,'test','test')", []any{id, h.memberID}},
		{"INSERT INTO checkout_runs(order_id,credentials_expires_at,hosted_url_enc,link_expires_at) VALUES($1,now()+interval '1 hour',$2,now()+interval '30 minutes')", []any{id, link}},
		{"INSERT INTO order_proxy_sessions(order_id,resource_id,endpoint_enc,expected_ip,expires_at) VALUES($1,'payment_proxy',$2,'203.0.113.2',now()+interval '1 hour')", []any{id, proxy}},
	} {
		if _, e := h.a.DB.Exec(ctx, q.s, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	p := &fakePayment{events: make(chan paymentEvent, 16)}
	h.a.paymentStart = func(_ context.Context, in paymentInput) (paymentProcess, error) {
		if in.Proxy == nil || in.ExpectedIP != "203.0.113.2" {
			t.Error("original order proxy not provided")
		}
		if in.Mode == "prepare" && (in.Card.CVC != "123" || !validBilling(in.Billing) || in.Billing["source"] != "saved") {
			t.Error("private runtime input missing")
		}
		return p, nil
	}
	t.Cleanup(func() {
		h.a.paymentMu.Lock()
		for _, s := range h.a.payments {
			s.cancel()
		}
		h.a.paymentMu.Unlock()
		time.Sleep(50 * time.Millisecond)
	})
	return h, id, p
}
func paymentPrepareBody() M {
	return M{"accepted": true, "card": M{"number": "4242424242424242", "month": "12", "year": "2035", "cvc": "123"}}
}
func waitPayment(t *testing.T, h *harness, id, state string) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		var s string
		h.a.DB.QueryRow(context.Background(), "SELECT state FROM payment_runs WHERE order_id=$1", id).Scan(&s)
		if s == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("payment did not become %s", state)
}
func paymentBalances(t *testing.T, h *harness, wantBalance, wantFrozen int64) {
	t.Helper()
	var b, f int64
	if e := h.a.DB.QueryRow(context.Background(), "SELECT balance,frozen FROM users WHERE id=$1", h.memberID).Scan(&b, &f); e != nil || b != wantBalance || f != wantFrozen {
		t.Fatalf("unexpected balance %d/%d expected %d/%d", b, f, wantBalance, wantFrozen)
	}
}
func TestPaymentPrepareConfirmAndExactlyOnceSettlement(t *testing.T) {
	h, id, p := paymentFixture(t)
	path := "/api/orders/" + id + "/payment/"
	expect(t, h.request("POST", path+"prepare", paymentPrepareBody(), h.other, nil), 404)
	result := expect(t, h.request("POST", path+"prepare", paymentPrepareBody(), h.member, nil), 202)
	attempt := str(result, "attempt_id")
	paymentBalances(t, h, 9900, 100)
	expect(t, h.request("POST", path+"prepare", paymentPrepareBody(), h.member, nil), 429)
	p.events <- paymentEvent{Type: "quote", Amount: 12345, Currency: "php"}
	waitPayment(t, h, id, "quoted")
	body := M{"attempt_id": attempt, "amount": 12345, "currency": "php", "accepted": true}
	badBody := M{"attempt_id": attempt, "amount": 1, "currency": "php", "accepted": true}
	expect(t, h.request("POST", path+"confirm", badBody, h.member, nil), 409)
	expect(t, h.request("POST", path+"confirm", body, h.other, nil), 404)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := h.request("POST", path+"confirm", body, h.member, nil)
			if w.Code != 200 && w.Code != 202 {
				t.Errorf("confirm %d", w.Code)
			}
		}()
	}
	wg.Wait()
	until := time.Now().Add(time.Second)
	for p.commits.Load() < 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if p.commits.Load() != 1 {
		t.Fatalf("sent %d confirmations", p.commits.Load())
	}
	expect(t, h.request("POST", path+"cancel", M{}, h.member, nil), 404)
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 12345, Currency: "php"}
	waitPayment(t, h, id, "succeeded")
	paymentBalances(t, h, 9900, 0)
	if e := h.a.finishPayment(context.Background(), id, attempt, paymentEvent{State: "succeeded", Amount: 12345, Currency: "php"}); e != nil {
		t.Fatal(e)
	}
	var charges int
	h.a.DB.QueryRow(context.Background(), "SELECT count(*) FROM ledger WHERE order_id=$1 AND kind='charge'", id).Scan(&charges)
	if charges != 1 {
		t.Fatal("duplicate charge")
	}
	for _, target := range []struct {
		path  string
		admin bool
	}{{"/api/orders/" + id, false}, {"/api/admin/orders/" + id, true}} {
		cookie := h.member
		if target.admin {
			cookie = h.admin
		}
		w := h.request("GET", target.path, nil, cookie, nil)
		expect(t, w, 200)
		for _, secret := range []string{"4242424242424242", "PRIVATE_PROXY", "payment_pages", "payment_methods", "cvc", "proxy_enc"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("private payment data leaked")
			}
		}
	}
	var dump string
	h.a.DB.QueryRow(context.Background(), "SELECT row_to_json(p)::text FROM payment_runs p WHERE order_id=$1", id).Scan(&dump)
	if strings.Contains(dump, "4242424242424242") || strings.Contains(dump, "PRIVATE_PROXY") {
		t.Fatal("card/proxy plaintext persisted")
	}
}
func TestPaymentTimeoutAndNoChargeReplay(t *testing.T) {
	for _, mode := range []string{"prepare_crash", "submitted_crash", "amount_change", "wrong_currency", "verification", "decline"} {
		t.Run(mode, func(t *testing.T) {
			h, id, p := paymentFixture(t)
			path := "/api/orders/" + id + "/payment/"
			v := expect(t, h.request("POST", path+"prepare", paymentPrepareBody(), h.member, nil), 202)
			attempt := str(v, "attempt_id")

			p.events <- paymentEvent{Type: "quote", Amount: 1000, Currency: "usd"}
			waitPayment(t, h, id, "quoted")
			if mode != "prepare_crash" {
				expect(t, h.request("POST", path+"confirm", M{"attempt_id": attempt, "amount": 1000, "currency": "usd", "accepted": true}, h.member, nil), 202)
			}
			if mode == "prepare_crash" || mode == "submitted_crash" {
				close(p.events)
			} else {
				ev := paymentEvent{Type: "result", Sent: true, State: "succeeded", Amount: 1000, Currency: "usd"}
				if mode == "wrong_currency" {
					ev.Currency = "php"
				}
				if mode == "amount_change" {
					ev.Amount = 1001
				}
				if mode == "verification" {
					ev.State = "action_required"
				}
				if mode == "decline" {
					ev.State = "declined"
					ev.Code = "payment_declined"
				}
				p.events <- ev
			}
			if mode == "prepare_crash" || mode == "decline" {
				waitPayment(t, h, id, "failed")
				paymentBalances(t, h, 10000, 0)
			} else {
				state := "review"
				if mode == "verification" {
					state = "action_required"
				}
				waitPayment(t, h, id, state)
				paymentBalances(t, h, 9900, 100)
				expect(t, h.request("POST", path+"prepare", paymentPrepareBody(), h.member, nil), 409)
			}
		})
	}
}
func TestPaymentReadOnlyRecoveryAndLeaseFencing(t *testing.T) {
	h, id, p := paymentFixture(t)
	ctx := context.Background()
	path := "/api/orders/" + id + "/payment/"
	v := expect(t, h.request("POST", path+"prepare", paymentPrepareBody(), h.member, nil), 202)
	attempt := str(v, "attempt_id")
	p.events <- paymentEvent{Type: "quote", Amount: 1000, Currency: "usd"}
	waitPayment(t, h, id, "quoted")
	expect(t, h.request("POST", path+"confirm", M{"attempt_id": attempt, "amount": 1000, "currency": "usd", "accepted": true}, h.member, nil), 202)
	p.events <- paymentEvent{Type: "result", State: "review", Sent: true}
	waitPayment(t, h, id, "review")
	// Original run is now finished; read-only recovery must never receive card fields.
	h.a.paymentStart = func(_ context.Context, in paymentInput) (paymentProcess, error) {
		if in.Mode != "check" || in.Card.Number != "" || in.Card.CVC != "" {
			t.Error("recovery can submit card")
		}
		q := &fakePayment{events: make(chan paymentEvent, 2)}
		q.events <- paymentEvent{Type: "result", State: "succeeded", Amount: 1000, Currency: "usd"}
		return q, nil
	}
	expect(t, h.request("POST", path+"check", M{}, h.other, nil), 404)
	expect(t, h.request("POST", "/api/admin/orders/"+id+"/payment/check", M{}, h.member, nil), 403)
	expect(t, h.request("POST", path+"check", M{}, h.member, nil), 202)
	waitPayment(t, h, id, "succeeded")
	paymentBalances(t, h, 9900, 0)
	if e := h.a.finishPayment(ctx, id, "stale-attempt", paymentEvent{State: "failed"}); !errors.Is(e, pgx.ErrNoRows) {
		t.Fatal("stale attempt accepted")
	}
}
func TestPaymentNeverFallsBackWithoutOrderProxy(t *testing.T) {
	h, id, _ := paymentFixture(t)
	h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
		t.Error("runtime started without original proxy")
		return nil, errors.New("blocked")
	}
	if _, e := h.a.DB.Exec(context.Background(), "DELETE FROM resources"); e != nil {
		t.Fatal(e)
	}
	expect(t, h.request("POST", "/api/orders/"+id+"/payment/prepare", paymentPrepareBody(), h.member, nil), 202)
	waitPayment(t, h, id, "failed")
	paymentBalances(t, h, 10000, 0)
	var leases int
	h.a.DB.QueryRow(context.Background(), "SELECT count(*) FROM proxy_leases").Scan(&leases)
	if leases != 0 {
		t.Fatal("failed payment leaked proxy capacity")
	}
}

func TestPaymentRuntimeInputAndCardValidation(t *testing.T) {
	c := cardInput{Number: "4242 4242 4242 4242", Month: "12", Year: "35", CVC: "123"}
	if !validCard(&c) || c.Year != "2035" {
		t.Fatal("valid card rejected")
	}
	c.Number = "4242424242424241"
	if validCard(&c) {
		t.Fatal("bad checksum accepted")
	}
	for _, raw := range []string{"https://evil.example/c/pay/cs_test_QA123", "https://pay.openai.com@evil.example/c/pay/cs_test_QA123", "https://pay.openai.com/c/pay/../../secret"} {
		if hostedPaymentURL(raw) {
			t.Fatal("unsafe hosted URL accepted")
		}
	}
	b, _ := json.Marshal(paymentEvent{Type: "stage", Code: "opening"})
	if strings.Contains(string(b), "proxy") {
		t.Fatal("event protocol leaks")
	}
}
