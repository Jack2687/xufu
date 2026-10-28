// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
)

// 付款接口按订单归属选择；Stripe 内部仍使用与上游会话匹配的表单。
func (t *gptpay_transaction) runCheckoutPayment(card model_Card, billing model_Billing, checkout, info protocol_Map) error {
	switch {
	case strings.HasPrefix(t.cp.CheckoutID, "oaics_"):
		return t.runCustomFlow(card, billing, checkout)
	case strings.HasPrefix(t.cp.CheckoutID, "cs_live_"):
		if t.cp.Metadata["checkout_ui_mode"] == "redirect" {
			return t.runHostedFlow(card, billing, info)
		}
		return t.runCustomStripeCheckout(card, billing, checkout)
	default:
		return protocol_NewError("checkout_id_invalid")
	}
}
