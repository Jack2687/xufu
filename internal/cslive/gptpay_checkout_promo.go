// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
	unicode "unicode"
	utf8 "unicode/utf8"
)

func gptpay_checkoutPromoCode(plan, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !model_IsTeamPlan(strings.ToLower(plan)) || !utf8.ValidString(value) || len(value) > 256 {
		return "", protocol_NewError("invalid_promo_code")
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return "", protocol_NewError("invalid_promo_code")
		}
	}
	return value, nil
}
