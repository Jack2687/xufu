// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	errors "errors"
	math "math"
	url "net/url"
	regexp "regexp"
	strings "strings"
	time "time"
)

type observability_Observer struct {
	Operation, TraceID    string
	sink                  model_EventSink
	started, phaseStarted time.Time
	phase                 string
}

func observability_NewObserver(operation string, sink model_EventSink, traceID, phase string) *observability_Observer {
	if !protocol_Matches(`[0-9a-f]{32}`, traceID) {
		traceID = strings.ReplaceAll(protocol_RandomUUID(), "-", "")
	}
	now := time.Now()
	return &observability_Observer{Operation: observability_safeString(operation), TraceID: traceID, sink: sink, started: now, phaseStarted: now, phase: phase}
}

var observability_eventFields = map[string]string{"checkout_prefix": "checkoutPrefix", "payment_route": "paymentRoute", "sdk_has_stripe_js": "sdkHasStripeJS", "sdk_has_three_d_secure_2_source": "sdkHasThreeDS2Source", "sdk_has_server_transaction_id": "sdkHasServerTransactionID", "sdk_has_directory_server_name": "sdkHasDirectoryServerName", "diagnostic_source": "diagnosticSource", "raw_status": "rawStatus", "raw_state": "rawState", "raw_payment_status": "rawPaymentStatus", "next_action_type": "nextActionType", "sdk_action_type": "sdkActionType", "network_stage": "networkStage", "connection_ready_ms": "connectionReadyMs", "request_write_ms": "requestWriteMs", "first_byte_ms": "firstByteMs", "response_wait_ms": "responseWaitMs", "network_stage_elapsed_ms": "networkStageElapsedMs", "plan_type": "planType", "renewal": "renewal", "active_until": "activeUntil", "attempt": "attempt", "retry_wait_ms": "retryWaitMs", "retry_wait_elapsed_ms": "retryWaitElapsedMs", "duration_ms": "durationMs", "payment_elapsed_ms": "paymentElapsedMs", "error_code": "errorCode", "warning_code": "warningCode", "provider_param": "providerParam", "provider_code": "providerCode", "decline_code": "declineCode", "request_id": "requestId", "executor": "executor", "flow": "flow", "intent_status": "intentStatus", "method": "method", "outcome": "outcome", "path": "path", "payment_state": "paymentState", "phase": "phase", "remaining_deadline_ms": "remainingDeadlineMs", "service": "service", "status": "status", "browser_profile": "browserProfile", "response_content_type": "responseContentType", "response_error_code": "responseErrorCode", "response_error_type": "responseErrorType", "response_message": "responseMessage", "response_detail": "responseDetail", "response_title": "responseTitle", "response_summary": "responseSummary", "response_truncated": "responseTruncated", "cf_ray": "cfRay", "response_request_id": "responseRequestId"}

var observability_safePattern = regexp.MustCompile(`[^A-Za-z0-9_.:/{}-]+`)

var observability_providerID = regexp.MustCompile(`(?:oaics_|cs_live_|cs_test_|pi_|seti_|pm_|ctoken_|bps_)[A-Za-z0-9_]+`)

func observability_safeString(s string) string {
	s = observability_safePattern.ReplaceAllString(s, "_")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func observability_NormalizeHTTPPath(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return "/"
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	// Customer Portal 的路径本身携带访问凭据，仅移除 query 仍会泄漏。
	if strings.HasPrefix(p, "/p/session/") {
		return "/p/session/{id}"
	}
	return observability_providerID.ReplaceAllString(p, "{id}")
}

var observability_knownErrors = protocol_StringSet("radar_session_missing", "billing_automatic_tax_unrecognized_customer_location", "parameter_unknown", "catalog_prices_invalid", "catalog_prices_region_mismatch", "protocol_error", "network_timeout", "account_mismatch", "account_not_found", "action_required", "activation_pending", "subscription_not_found", "approval_blocked", "authorized", "cancellation_pending", "charged", "checkout_amount_changed", "checkout_currency_mismatch", "checkout_id_invalid", "checkout_total_missing", "checkout_price_missing", "checkout_price_failed", "checkout_context_mismatch", "checkpoint_failed", "confirmation_token_missing", "deadline_exceeded", "device_id_missing", "existing_subscription", "homepage_unavailable", "hosted_page_unavailable", "hosted_url_invalid", "hosted_url_missing", "https_required", "init_checksum_missing", "invalid_json_response", "invalid_return_url", "network_error", "not_attempted", "not_charged", "payment_intent_secret_missing", "payment_intent_state_unavailable", "payment_confirmation_already_submitted", "payment_method_missing", "payment_page_id_missing", "payment_not_submitted", "payment_pending", "pending", "personal_account_required", "processor_entity_invalid", "publishable_key_missing", "recovery_failed", "renewal_state_unknown", "sentinel_challenge_missing", "sentinel_context_mismatch", "sentinel_dx_missing", "sentinel_incomplete", "sentinel_pow_missing", "sentinel_turnstile_error", "sentinel_turnstile_missing", "session_expired", "session_token_required", "stripe_build_missing", "stripe_identity_missing", "unexpected_redirect", "unknown", "unsupported_processor_entity", "checkout_ui_mode_invalid", "payment_approval_failed", "payment_cancelled", "input_or_checkpoint_invalid", "cancelled")

var observability_providerCodes = protocol_StringSet("billing_automatic_tax_unrecognized_customer_location", "parameter_unknown", "payment_method_failed", "payment_method_declined", "card_declined", "expired_card", "incorrect_cvc", "incorrect_number", "incorrect_zip", "invalid_cvc", "invalid_expiry_month", "invalid_expiry_year", "invalid_number", "processing_error", "payment_intent_unexpected_state", "payment_intent_authentication_failure", "authentication_required")

var observability_declineCodes = protocol_StringSet("payment_method_declined", "payment_method_failed", "card_declined", "do_not_honor", "generic_decline", "insufficient_funds", "duplicate_transaction", "transaction_not_allowed", "card_not_supported", "currency_not_supported", "expired_card", "incorrect_cvc", "incorrect_number", "incorrect_zip", "invalid_account", "invalid_amount", "issuer_not_available", "try_again_later", "processing_error", "reenter_transaction", "restricted_card", "fraudulent", "lost_card", "stolen_card", "merchant_blacklist", "authentication_required", "approve_with_id", "call_issuer", "card_velocity_exceeded", "no_action_taken", "not_permitted", "pickup_card", "revocation_of_authorization", "revocation_of_all_authorizations", "security_violation", "service_not_allowed", "stop_payment_order", "testmode_decline", "withdrawal_count_limit_exceeded")

func observability_SafeErrorCode(value any) string {
	var code string
	switch x := value.(type) {
	case string:
		code = x
	case error:
		var pe *protocol_ProtocolError
		if errors.As(x, &pe) {
			code = pe.Code
		} else {
			return "network_error"
		}
	default:
		return "redacted_error"
	}
	if observability_knownErrors[code] || protocol_Matches(`http_[1-5][0-9]{2}`, code) || protocol_Matches(`protocol_error_[A-Za-z][A-Za-z0-9]{0,63}`, code) {
		return code
	}
	return "redacted_error"
}

func (o *observability_Observer) Emit(event string, fields protocol_Map) {
	if o == nil {
		return
	}
	defer func() { _ = recover() }()
	now := time.Now()
	payload := model_Event{"timestamp": now.UTC().Format("2006-01-02T15:04:05.000+00:00"), "event": observability_safeString(event), "traceId": o.TraceID, "operation": o.Operation, "elapsedMs": math.RoundToEven(now.Sub(o.started).Seconds() * 1000)}
	for k, target := range observability_eventFields {
		v, ok := fields[k]
		if !ok || v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		switch k {
		case "checkout_prefix":
			if value, ok := v.(string); ok && protocol_StringSet("oaics_", "cs_live_")[value] {
				payload[target] = value
			}
		case "payment_route":
			if value, ok := v.(string); ok && protocol_StringSet("chatgpt", "stripe")[value] {
				payload[target] = value
			}
		case "response_truncated":
			if value, ok := v.(bool); ok {
				payload[target] = value
			}
		case "browser_profile":
			if value, ok := v.(string); ok && protocol_StringSet("chrome131-windows-v1", "chrome131-macos-v1")[value] {
				payload[target] = value
			}
		case "plan_type":
			if value, ok := v.(string); ok && (strings.ToLower(value) == "free" || model_IsSubscriptionType(strings.ToLower(value))) {
				payload[target] = strings.ToLower(value)
			}
		case "renewal":
			if value, ok := v.(string); ok && protocol_StringSet("on", "off", "unknown")[value] {
				payload[target] = value
			}
		case "active_until":
			if value, ok := v.(string); ok {
				if ts, err := time.Parse(time.RFC3339, value); err == nil {
					payload[target] = ts.UTC().Format("2006-01-02T15:04:05Z")
				}
			}
		case "flow":
			if value, ok := v.(string); ok && protocol_StringSet("checkout", "subscription_update", "existing_subscription")[value] {
				payload[target] = value
			} else {
				payload[target] = observability_safeString(protocol_AsString(v))
			}
		case "response_content_type":
			if value, ok := v.(string); ok {
				if media := observability_safeContentType(value); media != "" {
					payload[target] = media
				}
			}
		case "response_error_code", "response_error_type":
			if value, ok := v.(string); ok {
				payload[target] = observability_safeResponseCode(value, nil)
			}
		case "response_message", "response_detail", "response_title", "response_summary":
			if value, ok := v.(string); ok {
				payload[target] = observability_SafeResponseText(value, nil)
			}
		case "cf_ray":
			if value, ok := v.(string); ok && observability_diagnosticRay.MatchString(value) {
				payload[target] = value
			}
		case "response_request_id":
			if value, ok := v.(string); ok && observability_diagnosticRequestID.MatchString(value) {
				payload[target] = value
			}
		case "request_id":
			s, ok := v.(string)
			if ok && protocol_Matches(`req_[A-Za-z0-9]{1,80}`, s) {
				payload[target] = s
			} else {
				payload[target] = "redacted"
			}
		case "sdk_has_stripe_js", "sdk_has_three_d_secure_2_source", "sdk_has_server_transaction_id", "sdk_has_directory_server_name":
			if value, ok := v.(bool); ok {
				payload[target] = value
			}
		case "diagnostic_source":
			payload[target] = observability_SafeDiagnosticSource(protocol_AsString(v))
		case "raw_status", "raw_state", "raw_payment_status":
			payload[target] = observability_SafePaymentStatus(protocol_AsString(v))
		case "next_action_type", "sdk_action_type":
			payload[target] = observability_SafeActionType(protocol_AsString(v))
		case "network_stage":
			if value, ok := v.(string); ok && (value == "connection" || value == "request_write" || value == "response_headers" || value == "response_body") {
				payload[target] = value
			}
		case "provider_param":
			payload[target] = protocol_SafeProviderParam(protocol_AsString(v))
		case "provider_code", "decline_code":
			s, ok := v.(string)
			allowed := observability_providerCodes
			if k == "decline_code" {
				allowed = observability_declineCodes
			}
			if ok && allowed[s] {
				payload[target] = s
			} else {
				payload[target] = "redacted_error"
			}
		case "path":
			payload[target] = observability_NormalizeHTTPPath(protocol_AsString(v))
		case "error_code", "warning_code":
			payload[target] = observability_SafeErrorCode(v)
		case "payment_elapsed_ms":
			if elapsed, ok := observability_paymentElapsedMilliseconds(v); ok {
				payload[target] = elapsed
			}
		case "attempt", "status":
			if n, ok := protocol_Integer(v); ok {
				payload[target] = n
			}
		case "connection_ready_ms", "request_write_ms", "first_byte_ms", "response_wait_ms", "network_stage_elapsed_ms", "duration_ms", "remaining_deadline_ms", "retry_wait_ms", "retry_wait_elapsed_ms":
			var f float64
			switch x := v.(type) {
			case float64:
				f = x
			case int:
				f = float64(x)
			case int64:
				f = float64(x)
			}
			payload[target] = math.Max(0, math.RoundToEven(f))
		default:
			payload[target] = observability_safeString(protocol_AsString(v))
		}
	}
	if o.sink != nil {
		o.sink(payload)
	}
}

func (o *observability_Observer) Checkpoint(phase string, changed, persisted bool) {
	if changed {
		if o.phase != "" {
			o.Emit("phase", protocol_Map{"phase": o.phase, "outcome": "completed", "duration_ms": time.Since(o.phaseStarted).Seconds() * 1000})
		}
		o.phase = phase
		o.phaseStarted = time.Now()
	}
	outcome := "skipped"
	if persisted {
		outcome = "saved"
	}
	o.Emit("checkpoint", protocol_Map{"phase": phase, "outcome": outcome})
}

func (o *observability_Observer) Complete(phase, payment, code string, warnings ...string) {
	warning := ""
	if len(warnings) > 0 {
		warning = warnings[0]
	}
	// 已确认的扣款不因激活、续费或后续落盘失败改成支付失败。
	if payment == "charged" {
		if warning == "" {
			warning = code
		}
		code = ""
	}
	outcome := "succeeded"
	phaseOutcome := "completed"
	if code != "" {
		outcome = "failed"
		phaseOutcome = "failed"
	} else if warning != "" {
		phaseOutcome = "pending"
	}
	if o.phase != "" {
		o.Emit("phase", protocol_Map{"phase": o.phase, "outcome": phaseOutcome, "warning_code": warning, "duration_ms": time.Since(o.phaseStarted).Seconds() * 1000})
		o.phase = ""
	}
	o.Emit("transaction", protocol_Map{"phase": phase, "outcome": outcome, "payment_state": payment, "error_code": code, "warning_code": warning})
}

func observability_paymentElapsedMilliseconds(value any) (float64, bool) {
	var elapsed float64
	switch v := value.(type) {
	case float64:
		elapsed = v
	case int:
		elapsed = float64(v)
	case int64:
		elapsed = float64(v)
	default:
		return 0, false
	}
	if math.IsNaN(elapsed) || math.IsInf(elapsed, 0) {
		return 0, false
	}
	return math.Max(0, math.RoundToEven(elapsed)), true
}

// SafePaymentCode 只公开已有白名单错误码，不传递响应原文。
func observability_SafePaymentCode(code string, decline bool) string {
	allowed := observability_providerCodes
	if decline {
		allowed = observability_declineCodes
	}
	if allowed[code] {
		return code
	}
	return ""
}
