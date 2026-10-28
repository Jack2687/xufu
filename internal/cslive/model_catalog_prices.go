// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	time "time"
)

// CatalogPrices 是网页展示价快照，不是 Checkout 报价或付款授权。
// 年付节点保持原金额，未确认计价基准前不计算年付总额。
type model_CatalogPrices struct {
	ConfigKey         string               `json:"config_key"`
	Country           string               `json:"country"`
	Currency          string               `json:"currency"`
	MinorUnitExponent int                  `json:"minor_unit_exponent"`
	FetchedAt         time.Time            `json:"fetched_at"`
	DisplayOnly       bool                 `json:"display_only"`
	Items             []model_CatalogPrice `json:"items"`
}

type model_CatalogPrice struct {
	Plan        string `json:"plan"`
	PriceKey    string `json:"price_key"`
	Interval    string `json:"interval"`
	AmountMinor int64  `json:"amount_minor"`
	Tax         string `json:"tax"`
	// AmountBasis 仅在月付时为 month；年付保持 unverified，避免误乘或少算。
	AmountBasis string `json:"amount_basis"`
	PerSeat     bool   `json:"per_seat"`
}

// PricingConfigPlanKey 将 SDK 套餐 ID 映射到展示价格目录，不接受上游别名作为套餐 ID。
func model_PricingConfigPlanKey(plan string) string {
	switch plan {
	case "go", "plus", "promax":
		return plan
	case "pro5x":
		return "prolite"
	case "pro20x":
		return "pro"
	case "team":
		return "business"
	case "team5x":
		return "business_prolite"
	default:
		return ""
	}
}

// PricingConfigRegionKey 仅维护已核对的地区配置键；币种以响应为准，不复用付款地区默认值。
func model_PricingConfigRegionKey(country string) string {
	switch country {
	case "PH":
		return "PHP"
	case "JP":
		return "JPY"
	case "EG":
		return "EGP"
	case "US", "NR", "CL":
		return country
	default:
		return ""
	}
}
