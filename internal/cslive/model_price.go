// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
)

// Price 是定价或扣款阶段独立记录的金额快照。
type model_Price struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Source      string `json:"source"`
}

const (
	// PriceSourceCheckoutTax 表示金额来自普通 Checkout 流程的含税总价。
	model_PriceSourceCheckoutTax = "checkout_tax"
	// PriceSourceSubscriptionProration 表示金额来自 Plus 升级 Pro 5X 的按比例估算。
	model_PriceSourceSubscriptionProration = "subscription_proration"
	// PriceSourcePaymentIntentReceived 表示金额来自扣款确认时的 PaymentIntent。
	model_PriceSourcePaymentIntentReceived = "payment_intent_received"
)

// estimatedPrice、chargedPrice 各自独立存储于 Metadata，互不覆盖；二者在升级流程中可能不同。
func model_priceFromMetadata(m protocol_Map, charged bool) *model_Price {
	if model_validatePriceMetadata(m, charged) != nil {
		return nil
	}
	amount, _ := protocol_Integer(m["amount_minor"])
	currency := m["currency"].(string)
	source := m["source"].(string)
	return &model_Price{AmountMinor: amount, Currency: currency, Source: source}
}

// EstimatedPrice 返回定价阶段记录的金额；没有有效估价证据时为 nil。
func (c model_Checkpoint) EstimatedPrice() *model_Price {
	m, ok := c.Metadata["estimated_price"].(protocol_Map)
	if !ok {
		return nil
	}
	return model_priceFromMetadata(m, false)
}

// ChargedPrice 返回扣款确认阶段记录的实际金额；扣款确认前为 nil。
func (c model_Checkpoint) ChargedPrice() *model_Price {
	if c.Payment != string(model_PaymentCharged) {
		return nil
	}
	m, ok := c.Metadata["charged_price"].(protocol_Map)
	if !ok {
		return nil
	}
	return model_priceFromMetadata(m, true)
}

// EncodePriceMetadata 构造写入 Checkpoint.Metadata["estimated_price"/"charged_price"] 的结构；
// source 必须是 PriceSource* 常量之一，供 pkg/gptpay 在定价与扣款确认阶段调用。
func model_EncodePriceMetadata(amountMinor int64, currency, source string) protocol_Map {
	return protocol_Map{"amount_minor": amountMinor, "currency": currency, "source": source}
}

func model_validatePriceMetadata(v any, charged bool) error {
	m, ok := v.(protocol_Map)
	if !ok || len(m) != 3 {
		return fmt.Errorf("invalid checkpoint price metadata")
	}
	if n, ok := protocol_Integer(m["amount_minor"]); !ok || n < 0 {
		return fmt.Errorf("invalid checkpoint price amount")
	}
	if currency, ok := m["currency"].(string); !ok || !protocol_Matches(`[A-Z]{3}`, currency) {
		return fmt.Errorf("invalid checkpoint price currency")
	}
	source, ok := m["source"].(string)
	validSource := source == model_PriceSourceCheckoutTax || source == model_PriceSourceSubscriptionProration
	if charged {
		validSource = source == model_PriceSourcePaymentIntentReceived
	}
	if !ok || !validSource {
		return fmt.Errorf("invalid checkpoint price source")
	}
	return nil
}
