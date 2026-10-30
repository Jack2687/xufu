// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

// SafeActionType 只允许类型标签，未知值不携带验证链接或客户端密钥。
func observability_SafeActionType(value string) string {
	switch value {
	case "intent_confirmation_challenge", "stripe_js", "three_d_secure_2_fingerprint", "three_d_secure_2_challenge", "three_d_secure_2_source", "use_stripe_sdk", "redirect_to_url", "three_d_secure_redirect", "stripe_3ds2_fingerprint", "stripe_3ds2_challenge", "verify_with_microdeposits", "display_bank_transfer_instructions":
		return value
	default:
		return "redacted"
	}
}

// SafePaymentStatus 不输出未知的自由文本，保留排查所需的有限状态枚举。
func observability_SafePaymentStatus(value string) string {
	switch value {
	case "active", "open", "complete", "expired", "paid", "unpaid", "no_payment_required", "succeeded", "processing", "pending", "requires_payment_method", "requires_confirmation", "requires_action", "requires_capture", "requires_approval", "canceled", "cancelled", "declined", "failed", "blocked":
		return value
	default:
		return "redacted"
	}
}
func observability_SafeDiagnosticSource(value string) string {
	switch value {
	case "response", "payment_intent", "submission_payment_intent", "error_payment_intent", "submission_attempt":
		return value
	default:
		return "redacted"
	}
}
