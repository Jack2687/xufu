package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

var safeProviderCodes = map[string]bool{
	"account_deactivated": true, "account_deleted": true, "invalid_workspace_selected": true, "token_expired": true, "invalid_token": true, "RefreshAccessTokenError": true,
	"subscription_not_found": true, "card_declined": true, "expired_card": true, "incorrect_cvc": true, "incorrect_number": true, "incorrect_zip": true,
	"insufficient_funds": true, "do_not_honor": true, "generic_decline": true, "authentication_required": true, "payment_intent_authentication_failure": true,
	"payment_intent_unexpected_state": true, "billing_automatic_tax_unrecognized_customer_location": true, "rate_limit_exceeded": true, "processing_error": true,
}

func networkReason(err error) string {
	if err == nil {
		return ""
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "proxyconnect") || strings.Contains(message, "connect tunnel") {
		return "proxy_connect"
	}
	if strings.Contains(message, "tls") || strings.Contains(message, "certificate") {
		return "tls"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	return "connection"
}
func requestTimings(ctx context.Context) (context.Context, func() M) {
	var mu sync.Mutex
	starts := map[string]time.Time{}
	values := M{}
	began := time.Now()
	start := func(key string) { mu.Lock(); starts[key] = time.Now(); mu.Unlock() }
	end := func(key string) {
		mu.Lock()
		if at, ok := starts[key]; ok {
			values[key+"_ms"] = time.Since(at).Milliseconds()
		}
		mu.Unlock()
	}
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { start("dns") }, DNSDone: func(httptrace.DNSDoneInfo) { end("dns") },
		ConnectStart: func(string, string) { start("connect") }, ConnectDone: func(string, string, error) { end("connect") },
		TLSHandshakeStart: func() { start("tls") }, TLSHandshakeDone: func(tls.ConnectionState, error) { end("tls") },
		GotFirstResponseByte: func() { mu.Lock(); values["first_byte_ms"] = time.Since(began).Milliseconds(); mu.Unlock() },
	}
	return httptrace.WithClientTrace(ctx, trace), func() M {
		mu.Lock()
		defer mu.Unlock()
		out := M{}
		for k, v := range values {
			out[k] = v
		}
		return out
	}
}
func safeDiagnosticDetails(input M) M {
	out := M{}
	for k, v := range input {
		switch k {
		case "dns_ms", "connect_ms", "tls_ms", "first_byte_ms", "connection_ready_ms", "request_write_ms", "response_wait_ms", "network_stage_elapsed_ms", "request_attempt":
			var n int64
			switch x := v.(type) {
			case int:
				n = int64(x)
			case int64:
				n = x
			case float64:
				n = int64(x)
			default:
				continue
			}
			if n >= 0 && n <= 3600000 {
				if k == "request_attempt" && (n < 1 || n > 10) {
					continue
				}
				out[k] = n
			}
		case "network_stage":
			if s, ok := v.(string); ok && (s == "connection" || s == "request_write" || s == "response_headers" || s == "response_body") {
				out[k] = s
			}
		case "protocol_code":
			if s, ok := v.(string); ok && protocolDiagnosticCodes[s] {
				out[k] = s
			}
		case "network_reason":
			if s, ok := v.(string); ok && (s == "timeout" || s == "proxy_connect" || s == "tls" || s == "dns" || s == "connection") {
				out[k] = s
			}
		case "provider_code", "decline_code":
			if s, ok := v.(string); ok && safeProviderCodes[s] {
				out[k] = s
			}
		case "response_kind":
			if s, ok := v.(string); ok && (s == "json" || s == "html" || s == "empty" || s == "other") {
				out[k] = s
			}
		}
	}
	return out
}
