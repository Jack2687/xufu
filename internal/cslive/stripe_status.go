// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
)

// Classify 优先采用 PaymentIntent 的直接资金证据，避免页面状态掩盖已扣款或待操作状态。
func stripe_Classify(value any) model_PaymentState {
	data, ok := value.(map[string]any)
	if !ok {
		return model_PaymentUnknown
	}
	rootError := protocol_AsMap(data["error"])
	containers := []protocol_Map{data}
	for _, key := range []string{"error", "submission_attempt"} {
		if node := protocol_AsMap(data[key]); node != nil {
			containers = append(containers, node)
		}
	}
	for _, node := range containers {
		if pi := protocol_AsMap(node["payment_intent"]); pi != nil {
			if state := stripe_classifyIntent(pi, rootError); state != model_PaymentUnknown {
				return state
			}
		}
	}
	if strings.HasPrefix(protocol_AsString(data["id"]), "pi_") || data["object"] == "payment_intent" {
		if state := stripe_classifyIntent(data, rootError); state != model_PaymentUnknown {
			return state
		}
	}
	paymentStatus := stripe_normalizedStatus(data["payment_status"])
	if paymentStatus == "paid" {
		return model_PaymentCharged
	}
	submission := protocol_AsMap(data["submission_attempt"])
	topStates := []string{stripe_normalizedStatus(data["status"]), stripe_normalizedStatus(data["state"])}
	submissionStates := []string{stripe_normalizedStatus(submission["state"]), stripe_normalizedStatus(submission["status"])}
	states := append(append(append([]string{}, topStates...), submissionStates...), paymentStatus)
	if stripe_statusAny(states, "canceled", "cancelled", "declined", "failed", "expired") {
		return model_PaymentDeclined
	}
	if stripe_statusAny(states, "requires_approval") || stripe_statusAny(submissionStates, "requires_action") {
		return model_PaymentUnknown
	}
	if stripe_statusAny(topStates, "complete") && paymentStatus == "unpaid" {
		return model_PaymentDeclined
	}
	if stripe_statusAny(states, "open", "processing", "pending", "requires_confirmation") || paymentStatus == "unpaid" {
		return model_PaymentPending
	}
	if stripe_statusAny(topStates, "requires_action") {
		return model_PaymentActionRequired
	}
	errorData := protocol_AsMap(protocol_FirstTruthy(data["last_payment_error"], data["error"]))
	_, hasStatus := data["status"]
	_, hasState := data["state"]
	if (hasStatus || hasState) && stripe_statusAny(states, "requires_payment_method", "") && (protocol_Truthy(errorData["decline_code"]) || errorData["code"] == "card_declined") {
		return model_PaymentDeclined
	}
	return model_PaymentUnknown
}

func stripe_classifyIntent(pi, fallback protocol_Map) model_PaymentState {
	status := stripe_normalizedStatus(pi["status"])
	switch status {
	case "succeeded":
		return model_PaymentCharged
	case "requires_capture":
		return model_PaymentAuthorized
	case "requires_action":
		return model_PaymentActionRequired
	case "processing", "requires_confirmation":
		return model_PaymentPending
	case "canceled":
		return model_PaymentDeclined
	}
	// 验证失败已退回付款方式选择，不再是等待挑战；仅采用 PI 自身的明确错误。
	// 不根据 HTTP 错误或历史错误覆盖 processing、requires_capture、succeeded。
	if status == "requires_payment_method" && protocol_AsMap(pi["last_payment_error"])["code"] == "payment_intent_authentication_failure" {
		return model_PaymentDeclined
	}
	errorData := protocol_AsMap(protocol_FirstTruthy(pi["last_payment_error"], fallback))
	_, hasStatus := pi["status"]
	if hasStatus && (status == "requires_payment_method" || status == "") && (protocol_Truthy(errorData["decline_code"]) || errorData["code"] == "card_declined") {
		return model_PaymentDeclined
	}
	return model_PaymentUnknown
}

func stripe_normalizedStatus(v any) string {
	return strings.ToLower(strings.TrimSpace(protocol_AsString(protocol_FirstTruthy(v, ""))))
}

func stripe_statusAny(values []string, targets ...string) bool {
	for _, value := range values {
		for _, target := range targets {
			if value == target {
				return true
			}
		}
	}
	return false
}
