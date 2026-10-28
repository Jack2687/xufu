// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
)

// 仅替换已校验完整长链的域名，保留路径及 fragment 的原始编码。
func gptpay_publicCheckoutURL(raw, id string) string {
	if !strings.HasPrefix(id, "cs_live_") || stripe_FindHostedURL(raw, id) == "" {
		return raw
	}
	return strings.Replace(raw, "https://checkout.stripe.com/", "https://pay.openai.com/", 1)
}
