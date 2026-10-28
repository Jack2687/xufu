// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
	time "time"
)

// setPaymentState 冻结首次取得明确扣款证据时的耗时；之后的到账核验不改写资金结果。
func (t *gptpay_transaction) setPaymentState(state model_PaymentState) {
	if t.cp.Payment == "charged" {
		return
	}
	t.cp.Payment = string(state)
	if state != model_PaymentCharged {
		return
	}
	fields := protocol_Map{"phase": t.cp.Phase, "outcome": "succeeded", "payment_state": "charged"}
	// 恢复时只能确认当前状态，无法据此还原原交易实际扣款时刻。
	if t.observer.Operation == "pay" {
		elapsed := max(int64(0), time.Since(t.started).Milliseconds())
		t.cp.Metadata["payment_elapsed_ms"] = elapsed
		fields["payment_elapsed_ms"] = elapsed
	}
	t.observer.Emit("payment_confirm", fields)
	t.progress.emit(model_ProgressStagePayment, model_ProgressSucceeded, "")
}

func (t *gptpay_transaction) observePayment(data protocol_Map) {
	t.observePaymentDiagnostics(data)
	t.absorbHostedIntent(data)
	t.setPaymentState(stripe_Classify(data))
	t.recordChargedPrice(data)
}

// 实收只取原 PaymentIntent 的直接证据，估价与页面 unpaid 均不能替代资金事实。
func (t *gptpay_transaction) recordChargedPrice(data protocol_Map) {
	if t.cp.Payment != "charged" || t.cp.ChargedPrice() != nil || t.cp.PaymentIntentID == "" {
		return
	}
	intents := []protocol_Map{data, protocol_AsMap(data["payment_intent"]), protocol_AsMap(protocol_AsMap(data["submission_attempt"])["payment_intent"]), protocol_AsMap(protocol_AsMap(data["error"])["payment_intent"])}
	for _, intent := range intents {
		if intent["id"] != t.cp.PaymentIntentID || intent["status"] != "succeeded" {
			continue
		}
		amount, valid := protocol_Integer(intent["amount_received"])
		currency := strings.ToUpper(protocol_AsString(intent["currency"]))
		if valid && amount >= 0 && currency == t.cp.Currency {
			t.cp.Metadata["charged_price"] = model_EncodePriceMetadata(amount, currency, model_PriceSourcePaymentIntentReceived)
			return
		}
	}
}
