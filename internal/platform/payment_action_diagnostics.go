package platform

import (
	"regexp"
	"strings"
)

var actionResourceKey = regexp.MustCompile(`^[0-9a-f]{16}$`)

var actionDiagnosticEnums = map[string]string{
	"sdk_stage":         "browser_start page_loading sdk_loaded context_ready preflight preflight_returned preflight_error sdk_action sdk_returned sdk_exception verifying notifying",
	"sdk_resource":      "sdk_script sdk_frame processor_api verification_resource",
	"sdk_event":         "progress diagnostic network",
	"sdk_issue":         "csp_blocked resource_failed resource_http_error resource_completed resource_pending guard_timeout page_error",
	"sdk_host":          "js.stripe.com api.stripe.com hooks.stripe.com m.stripe.com q.stripe.com r.stripe.com m.stripe.network b.stripecdn.com hcaptcha.com js.hcaptcha.com newassets.hcaptcha.com imgs.hcaptcha.com api.hcaptcha.com accounts.hcaptcha.com challenges.cloudflare.com *.stripe.com *.stripe.network *.stripecdn.com *.hcaptcha.com *.cloudflare.com other",
	"sdk_route":         "stripe_v3 stripe_basil payment_intent three_ds static_asset other",
	"sdk_method":        "GET POST OPTIONS HEAD other",
	"sdk_request_type":  "document script stylesheet image font xhr fetch other",
	"sdk_network_error": "ERR_ABORTED ERR_FAILED ERR_TIMED_OUT ERR_CONNECTION_TIMED_OUT ERR_CONNECTION_CLOSED ERR_CONNECTION_RESET ERR_CONNECTION_REFUSED ERR_NAME_NOT_RESOLVED ERR_TUNNEL_CONNECTION_FAILED ERR_PROXY_CONNECTION_FAILED ERR_PROXY_AUTH_UNSUPPORTED ERR_INVALID_AUTH_CREDENTIALS ERR_HTTP_RESPONSE_CODE_FAILURE ERR_HTTP2_PROTOCOL_ERROR ERR_HTTP2_SERVER_REFUSED_STREAM ERR_QUIC_PROTOCOL_ERROR ERR_SSL_PROTOCOL_ERROR ERR_CERT_AUTHORITY_INVALID ERR_CERT_DATE_INVALID ERR_BLOCKED_BY_CLIENT ERR_BLOCKED_BY_RESPONSE ERR_NETWORK_CHANGED ERR_INTERNET_DISCONNECTED ERR_EMPTY_RESPONSE ERR_CONTENT_LENGTH_MISMATCH ERR_CONTENT_DECODING_FAILED unknown",
}

func safeActionDiagnostic(key string, value any) string {
	s, ok := value.(string)
	if ok && s != "" {
		for _, allowed := range strings.Fields(actionDiagnosticEnums[key]) {
			if s == allowed {
				return s
			}
		}
	}
	return ""
}
