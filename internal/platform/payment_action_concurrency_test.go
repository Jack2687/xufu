package platform

import (
	"context"
	"testing"
	"time"
	"xupay/internal/cslive"
)

type burstAction struct {
	events  chan actionEvent
	allowed int
}

func (p *burstAction) Events() <-chan actionEvent { return p.events }
func (p *burstAction) Close()                     {}
func (p *burstAction) Send(c actionCommand) error {
	if c.Type == "guard" && c.Allow {
		p.allowed++
		if p.allowed == 24 {
			p.events <- actionEvent{Type: "result", Outcome: "sdk_returned"}
		}
	}
	return nil
}

// Concurrent SDK resources receive live lease checks without network probes.
func TestActionConcurrentResourcesCheckLease(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	if ok, e := r.approve(1500, "PHP"); !ok || e != nil {
		t.Fatal(e)
	}
	cp.Payment, cp.Phase = "action_required", "payment_result"
	if e := r.save(cp); e != nil {
		t.Fatal(e)
	}
	if _, e := h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expected_ip='203.0.113.2' WHERE order_id=$1", r.id); e != nil {
		t.Fatal(e)
	}
	p := &burstAction{events: make(chan actionEvent, 32)}
	h.a.actionStart = func(context.Context, actionInput) (actionProcess, error) {
		for i := 1; i <= 24; i++ {
			p.events <- actionEvent{Type: "guard", RequestID: i}
		}
		return p, nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, 1500*time.Millisecond)
	defer cancel()
	r.ctx = ctx
	if err := r.runPaymentAction(cp, &paymentAction{automatic: true}); err != nil {
		t.Fatal(err)
	}
	if p.allowed != 24 {
		t.Fatalf("allowed=%d", p.allowed)
	}
	r.finish(cslive.Result{Checkpoint: cp})
}
