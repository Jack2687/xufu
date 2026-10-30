package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"xupay/internal/cslive"
)

type fixtureProtocolClient struct {
	pay     func(context.Context, cslive.Session, cslive.Card, cslive.PayOptions) (cslive.Result, error)
	recover func(context.Context, cslive.Checkpoint) (cslive.Result, error)
	proxy   string
}

func (c fixtureProtocolClient) Pay(ctx context.Context, s cslive.Session, card cslive.Card, o cslive.PayOptions) (cslive.Result, error) {
	return c.pay(ctx, s, card, o)
}
func (c fixtureProtocolClient) Recover(ctx context.Context, cp cslive.Checkpoint) (cslive.Result, error) {
	return c.recover(ctx, cp)
}
func (c fixtureProtocolClient) Proxy() string { return c.proxy }

// Starts at the real admitted order/worker boundary, so this catches a missing
// Pay -> SDK link even when the browser and recovery unit tests both pass.
func TestSubscriptionWorkerAutomaticallyContinuesOriginalPayment(t *testing.T) {
	for _, mode := range []string{"success", "keep", "still_required", "sdk_error", "runtime_error", "timeout", "ip_drift", "closed", "direct_success", "declined"} {
		t.Run(mode, func(t *testing.T) {
			h, fixture, cp := protocolFixture(t, mode == "keep", true)
			id, ctx := fixture.id, context.Background()
			checkoutType := "hosted"
			if mode == "keep" {
				checkoutType = "oaics"
				if _, err := h.a.DB.Exec(ctx, "UPDATE checkout_runs SET checkout_type=$2 WHERE order_id=$1", id, checkoutType); err != nil {
					t.Fatal(err)
				}
			}
			setCheckoutType(t, h, map[string]string{"auto": "oaics", "hosted": "oaics", "oaics": "hosted"}[checkoutType])
			var originalProxy, originalBilling string
			if err := h.a.DB.QueryRow(ctx, "SELECT endpoint_enc,details_enc FROM order_proxy_sessions JOIN order_billing USING(order_id) WHERE order_id=$1", id).Scan(&originalProxy, &originalBilling); err != nil {
				t.Fatal(err)
			}
			if _, err := h.a.DB.Exec(ctx, "UPDATE order_proxy_sessions SET expected_ip='203.0.113.2' WHERE order_id=$1", id); err != nil {
				t.Fatal(err)
			}
			pays, actions, recovers := 0, 0, 0
			var run *protocolRun
			process := &fixtureAction{events: make(chan actionEvent, 4)}
			h.a.actionStart = func(actionCtx context.Context, in actionInput) (actionProcess, error) {
				actions++
				if !in.Automatic || in.ClientSecret != cp.ClientSecret {
					t.Fatal("not automatic/original context")
				}
				deadline, _ := actionCtx.Deadline()
				if remaining := time.Until(deadline); remaining < 9*time.Minute || remaining > 10*time.Minute {
					t.Fatal("lost SDK window", remaining)
				}
				var state string
				var leased bool
				if err := h.a.DB.QueryRow(ctx, "SELECT p.state,s.lease_until>now() FROM payment_runs p JOIN subscription_protocol s USING(order_id) WHERE order_id=$1", id).Scan(&state, &leased); err != nil || state != "checking" || !leased {
					t.Fatal("SDK outside active task", state, err)
				}
				expect(t, h.request("POST", "/api/admin/orders/"+id+"/payment/action", M{}, h.admin, nil), 409)
				if mode == "runtime_error" {
					return nil, errors.New("PRIVATE_RUNTIME_FAILURE")
				}
				if mode == "closed" {
					expect(t, h.request("POST", "/api/admin/orders/"+id+"/close", M{"accepted": true, "reason": "Stop offline verification fixture"}, h.admin, nil), 200)
				}
				process.events <- actionEvent{Type: "guard", RequestID: 1}
				if mode == "timeout" {
					process.events <- actionEvent{Type: "timeout"}
				} else {
					outcome, code := "sdk_returned", ""
					if mode == "sdk_error" {
						outcome, code = "sdk_error", "card_declined"
					}
					process.events <- actionEvent{Type: "result", Outcome: outcome, Code: code}
				}
				close(process.events)
				return process, nil
			}
			h.a.protocolClient = func(r *protocolRun, ttl int) (protocolPaymentClient, func(), error) {
				run = r
				if ttl < 840 {
					t.Fatal("worker has no SDK/recovery budget", ttl)
				}
				client := fixtureProtocolClient{proxy: cp.Proxy}
				client.pay = func(_ context.Context, s cslive.Session, card cslive.Card, o cslive.PayOptions) (cslive.Result, error) {
					pays++
					if card.Number != "4242424242424242" || o.CheckoutType != protocolCheckoutType(checkoutType) || o.Plan != "pro20x" || s.SessionToken == "" || *o.CancelRenewal != (mode != "keep") {
						t.Fatal("changed Pay input")
					}
					var billing M
					if raw, e := h.a.decrypt(originalBilling); e != nil {
						t.Fatal(e)
					} else if e = json.Unmarshal([]byte(raw), &billing); e != nil {
						t.Fatal(e)
					}
					if o.Billing.Line1 != str(billing, "line1") || o.Billing.PostalCode != str(billing, "postal_code") || o.Billing.Country != str(billing, "country") {
						t.Fatal("random billing snapshot replaced")
					}
					if ok, e := o.ApprovePayment(1500, "PHP"); !ok || e != nil {
						t.Fatal("approval", e)
					}
					cp.Payment, cp.Phase = "action_required", "payment_result"
					if mode == "direct_success" {
						cp = nativeCharged(cp, 1500)
						cp.Activation, cp.Renewal, cp.Phase = "active", "off", "completed"
					}
					if mode == "declined" {
						cp.Payment = "not_charged"
					}
					if e := r.save(cp); e != nil {
						t.Fatal(e)
					}
					return cslive.Result{Checkpoint: cp}, nil
				}
				client.recover = func(_ context.Context, saved cslive.Checkpoint) (cslive.Result, error) {
					recovers++
					if actions != 1 || pays != 1 || saved.CheckoutID != cp.CheckoutID || saved.PaymentIntentID != cp.PaymentIntentID || saved.ClientSecret != cp.ClientSecret {
						t.Fatal("recovery replaced/repeated payment")
					}
					paymentBalances(t, h, 90000, 10000) // SDK callback did not settle money.
					if mode != "still_required" && mode != "sdk_error" {
						saved = nativeCharged(saved, 1500)
						saved.Activation, saved.Renewal, saved.Phase = "active", "off", "completed"
					}
					if e := r.save(saved); e != nil {
						t.Fatal(e)
					}
					return cslive.Result{Checkpoint: saved}, nil
				}
				return client, func() {}, nil
			}
			h.a.executeProtocolSubscription(ctx, fixture.job)
			if pays != 1 {
				t.Fatal("Pay not called exactly once", pays)
			}
			wantActions, wantRecovers := 1, 1
			if mode == "direct_success" || mode == "declined" {
				wantActions, wantRecovers = 0, 0
			}
			if mode == "runtime_error" || mode == "timeout" || mode == "closed" {
				wantRecovers = 0
			}
			if actions != wantActions || recovers != wantRecovers {
				t.Fatal("wrong continuation counts", actions, recovers)
			}
			want, balance, frozen := "action_required", int64(90000), int64(10000)
			if mode == "success" || mode == "keep" || mode == "direct_success" || mode == "ip_drift" {
				want, frozen = "succeeded", 0
			}
			if mode == "declined" {
				want, balance, frozen = "failed", 100000, 0
			}
			if mode == "closed" {
				want, balance, frozen = "cancelled", 100000, 0
			}
			waitPayment(t, h, id, want)
			paymentBalances(t, h, balance, frozen)
			var afterProxy, afterBilling, activation, renewal string
			if err := h.a.DB.QueryRow(ctx, "SELECT endpoint_enc,details_enc,o.activation,o.renewal FROM order_proxy_sessions JOIN order_billing USING(order_id) JOIN orders o ON o.id=order_id WHERE order_id=$1", id).Scan(&afterProxy, &afterBilling, &activation, &renewal); err != nil {
				t.Fatal(err)
			}
			if afterProxy != originalProxy || afterBilling != originalBilling {
				t.Fatal("order identity changed")
			}
			if want == "succeeded" && (activation != "active" || (mode == "keep" && renewal != "kept") || (mode != "keep" && renewal != "disabled")) {
				t.Fatal("unfinished lifecycle", activation, renewal)
			}
			if run == nil {
				t.Fatal("worker not reached")
			}
			view := h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil).Body.String()
			if wantActions > 0 && !strings.Contains(view, "automatic_verification") {
				t.Fatal("automatic start not monitored")
			}
			for _, secret := range []string{cp.ClientSecret, "TEST_PRIVATE", "PRIVATE_RUNTIME_FAILURE"} {
				if strings.Contains(view, secret) {
					t.Fatal("secret in monitor")
				}
			}
			if actions > 0 && mode != "runtime_error" && !process.closed {
				t.Fatal("SDK process leaked")
			}
		})
	}
}

func TestProtocolRecoveryAutomaticallyContinuesPendingSDK(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	if ok, err := r.approve(1500, "PHP"); !ok || err != nil {
		t.Fatal(err)
	}
	cp.Payment, cp.Phase = "action_required", "payment_result"
	cp.Metadata["browser_profile"] = "chrome131-windows-v1"
	cp.Metadata["hosted_url"] = "https://pay.openai.com/c/pay/" + cp.CheckoutID + "#" + strings.Repeat("a", 300)
	cp.DeviceID, cp.StripeVersion = "11111111-1111-4111-8111-111111111111", "2025-03-31.basil"
	cp.StripeIdentity = map[string]string{"guid": "fixture-guid", "muid": "fixture-muid", "sid": "fixture-sid"}
	if err := cp.ValidateForRecovery(); err != nil {
		t.Fatal(err)
	}
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expected_ip='203.0.113.2' WHERE order_id=$1", r.id); err != nil {
		t.Fatal(err)
	}
	r.finish(cslive.Result{Checkpoint: cp})
	finished := make(chan struct{})
	actions, reads := 0, 0
	h.a.actionStart = func(_ context.Context, in actionInput) (actionProcess, error) {
		actions++
		if !in.Automatic || in.ClientSecret != cp.ClientSecret {
			t.Error("recovery lost automatic original context")
		}
		p := &fixtureAction{events: make(chan actionEvent, 1)}
		p.events <- actionEvent{Type: "result", Outcome: "sdk_returned"}
		close(p.events)
		return p, nil
	}
	h.a.protocolClient = func(run *protocolRun, _ int) (protocolPaymentClient, func(), error) {
		client := fixtureProtocolClient{proxy: cp.Proxy, pay: func(context.Context, cslive.Session, cslive.Card, cslive.PayOptions) (cslive.Result, error) {
			t.Error("recovery repeated Pay")
			return cslive.Result{}, errors.New("no replay")
		}}
		client.recover = func(_ context.Context, saved cslive.Checkpoint) (cslive.Result, error) {
			reads++
			if saved.PaymentIntentID != cp.PaymentIntentID {
				t.Error("wrong original intent")
			}
			if reads == 2 {
				saved = nativeCharged(saved, 1500)
				saved.Activation, saved.Renewal, saved.Phase = "active", "off", "completed"
			}
			if reads > 2 {
				t.Error("repeated SDK recovery")
			}
			if err := run.save(saved); err != nil {
				return cslive.Result{}, err
			}
			return cslive.Result{Checkpoint: saved}, nil
		}
		return client, func() { close(finished) }, nil
	}
	expect(t, h.request("POST", "/api/admin/orders/"+r.id+"/payment/check", M{}, h.admin, nil), 202)
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic recovery did not finish")
	}
	if actions != 1 || reads != 2 {
		t.Fatal("recovery did not read/verify/reread", actions, reads)
	}
	waitPayment(t, h, r.id, "succeeded")
	paymentBalances(t, h, 90000, 0)
}

func TestProtocolClientPreservesReferenceOperationTimeout(t *testing.T) {
	_, run, _ := nativeFixture(t, false)
	c, closeClient, err := run.client(1200)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	config := c.(nativeProtocolClient).Config
	defaults := cslive.DefaultConfig()
	if config.Timeout != defaults.Timeout || config.Deadline != defaults.Deadline || config.BrowserProfile != defaults.BrowserProfile {
		t.Fatal("worker budget changed protocol defaults")
	}
}
