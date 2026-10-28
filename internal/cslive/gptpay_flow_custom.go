// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	url "net/url"
	strings "strings"
)

func (t *gptpay_transaction) runCustomFlow(card model_Card, billing model_Billing, checkout protocol_Map) error {
	if strings.HasPrefix(t.cp.CheckoutID, "cs_live_") {
		return t.runCustomStripeCheckout(card, billing, checkout)
	}
	token, e := t.stripe.Token(card, billing, checkout)
	if e != nil {
		return e
	}
	t.cp.ConfirmationToken = token
	return t.runCustomInternalConfirm()
}

func (t *gptpay_transaction) runCustomInternalConfirm() error {
	cp := t.cp
	proof, e := t.sentinel.Token("checkout_session_approval")
	if e != nil {
		return e
	}
	path := "/backend-api/payments/checkout/confirm"
	headers, e := t.platform.ChallengeHeaders(path, proof, chatgpt_PlatformBase+"/checkout/"+cp.Entity+"/"+cp.CheckoutID, false)
	if e != nil {
		return e
	}
	if e = t.boundary("openai_confirm_checkout"); e != nil {
		return e
	}
	data, e := t.platform.Call("POST", path, chatgpt_PlatformCallOptions{Body: protocol_O("checkout_session_id", cp.CheckoutID, "confirm_token", cp.ConfirmationToken, "selected_payment_method_type", "card"), Headers: headers})
	if e != nil {
		return e
	}
	status := strings.ToLower(protocol_AsString(protocol_FirstTruthy(data["status"], data["result"], data["state"])))
	if status == "blocked" {
		t.setPaymentState(model_PaymentDeclined)
		if e = t.save("payment_result"); e != nil {
			return e
		}
		return protocol_NewError("approval_blocked")
	}
	cp.ClientSecret = protocol_AsString(protocol_FirstTruthy(
		data["client_secret"],
		protocol_AsMap(data["payment_intent"])["client_secret"],
		protocol_AsMap(data["stripe_payment_intent"])["client_secret"],
		protocol_AsMap(data["checkout_session"])["client_secret"],
	))
	if !protocol_Matches(`pi_[A-Za-z0-9]+_secret_[A-Za-z0-9]+`, cp.ClientSecret) {
		return protocol_NewError("payment_intent_secret_missing")
	}
	cp.PaymentIntentID = strings.Split(cp.ClientSecret, "_secret_")[0]
	if raw := protocol_AsString(protocol_FirstTruthy(data["confirm_return_url"], cp.ReturnURL)); raw != "" {
		cp.ReturnURL = raw
	}
	if e = t.save("openai_confirmed"); e != nil {
		return e
	}
	if _, e = chatgpt_ValidateReturnURL(cp.ReturnURL, cp); e != nil {
		return e
	}
	ready, e := t.needsClientConfirmation()
	if e != nil {
		return e
	}
	if !ready {
		return t.save("payment_result")
	}
	cp.ConfirmBody = t.stripe.ConfirmForm()
	if e = t.boundary("stripe_confirm"); e != nil {
		return e
	}
	data, e = t.stripe.Confirm()
	if e != nil {
		return e
	}
	t.observePayment(data)
	return t.save("payment_result")
}

func (t *gptpay_transaction) runCustomStripeCheckout(card model_Card, billing model_Billing, checkout protocol_Map) error {
	cp := t.cp
	init, e := t.stripe.CustomPriceSnapshot(checkout)
	if e != nil {
		return e
	}
	if protocol_AsString(init["init_checksum"]) == "" {
		init, e = t.stripe.CustomPage()
		if e != nil {
			return e
		}
	}
	if e = t.stripe.BindCheckoutElements(checkout); e != nil {
		return e
	}
	updated, e := t.stripe.UpdateCustomCheckout(billing, init)
	if e != nil {
		return e
	}
	stripe_MergeCustomPaymentPage(init, updated)
	if e = t.stripe.CreateRadarSession(); e != nil {
		t.observer.Emit("radar_session", protocol_Map{"phase": t.cp.Phase, "outcome": "failed", "error_code": protocol_FlowErrorCode(e)})
	} else {
		t.observer.Emit("radar_session", protocol_Map{"phase": t.cp.Phase, "outcome": "succeeded"})
	}
	token, e := t.stripe.Token(card, billing, checkout)
	if e != nil {
		return e
	}
	cp.ConfirmationToken = token
	if cp.ReturnURL == "" {
		cp.ReturnURL = chatgpt_PlatformBase + "/checkout/verify?stripe_session_id=" + url.QueryEscape(cp.CheckoutID) + "&processor_entity=" + url.QueryEscape(cp.Entity) + "&plan_type=" + model_HostedPlanType(cp.Plan)
	}
	if _, e = chatgpt_ValidateReturnURL(cp.ReturnURL, cp); e != nil {
		return e
	}
	form, e := t.stripe.CustomCheckoutForm(init, "")
	if e != nil {
		return e
	}
	cp.ConfirmBody = form
	if e = t.boundary("openai_confirm_checkout"); e != nil {
		return e
	}
	data, e := t.stripe.ConfirmCheckoutSession()
	if e != nil {
		return e
	}
	t.observePaymentDiagnostics(data)
	errObj := protocol_AsMap(data["error"])
	t.observer.Emit("payment_confirm", protocol_Map{"phase": cp.Phase, "outcome": "submitted", "intent_status": protocol_AsString(protocol_FirstTruthy(data["payment_status"], data["status"])), "checkout_status": protocol_AsString(data["status"]), "payment_status": protocol_AsString(data["payment_status"]), "page_id_kind": gptpay_paymentPageIDKind(data), "has_payment_intent": gptpay_hasPaymentIntent(data), "provider_code": protocol_AsString(errObj["code"]), "provider_param": protocol_AsString(errObj["param"])})
	if code := protocol_AsString(errObj["code"]); code != "" {
		page, pageErr := t.stripe.CustomPage()
		if pageErr == nil {
			data = page
			t.observePayment(data)
		}
		if e = t.save("payment_result"); e != nil {
			return e
		}
		return protocol_NewError(code)
	}
	data, e = t.pollCustomPayment(data)
	if e != nil {
		return e
	}
	submission := protocol_AsMap(data["submission_attempt"])
	if protocol_FirstTruthy(submission["state"], submission["status"]) == "requires_approval" || protocol_FirstTruthy(submission["state"], submission["status"]) == "requires_action" {
		if e = t.snapshotHostedBilling(billing); e != nil {
			return e
		}
	}
	data, e = t.approveCustomCheckout(data)
	if e != nil {
		return e
	}
	// 先保存资金证据，再报告提交失败原因，不能把税务错误隐藏成通用未扣款。
	t.observePayment(data)
	if e = t.save("payment_result"); e != nil {
		return e
	}
	return gptpay_customSubmissionError(data)
}

func (t *gptpay_transaction) pollCustomPayment(data protocol_Map) (protocol_Map, error) {
	var e error
	for i := 0; i < 8; i++ {
		state := stripe_Classify(data)
		submission := protocol_AsMap(data["submission_attempt"])
		if state == model_PaymentCharged || state == model_PaymentActionRequired || state == model_PaymentDeclined || protocol_FirstTruthy(submission["state"], submission["status"]) == "requires_approval" {
			break
		}
		if e = t.stripeHTTP.Pause(1.2); e != nil {
			return nil, e
		}
		data, e = t.stripe.CustomPage()
		if e != nil {
			return nil, e
		}
	}
	return data, nil
}

func (t *gptpay_transaction) approveCustomCheckout(data protocol_Map) (protocol_Map, error) {
	cp := t.cp
	submission := protocol_AsMap(data["submission_attempt"])
	state := protocol_FirstTruthy(submission["state"], submission["status"])
	if state != "requires_approval" && state != "requires_action" {
		return data, nil
	}
	path := "/backend-api/payments/checkout/approve"
	token, e := t.sentinel.Token("checkout_session_approval")
	if e != nil {
		return nil, e
	}
	headers, e := t.platform.ChallengeHeaders(path, token, cp.ReturnURL, true)
	if e != nil {
		return nil, e
	}
	// 批准会推进原付款；挑战准备完成后再次同步持久化，校验调用方当前执行权。
	// 沿用原阶段，避免把待批准快照变为另一笔交易或不可恢复的阶段。
	if e = t.save(""); e != nil {
		return nil, e
	}
	reply, e := t.platform.Call("POST", path, chatgpt_PlatformCallOptions{Body: protocol_O("checkout_session_id", cp.CheckoutID, "processor_entity", cp.Entity), Headers: headers})
	if e != nil {
		return nil, e
	}
	if protocol_FirstTruthy(reply["result"], reply["status"], reply["state"]) == "blocked" {
		return nil, protocol_NewError("approval_blocked")
	}
	data, e = t.stripe.CustomPage()
	if e != nil {
		return nil, e
	}
	return t.pollCustomPaymentAfterApprove(data)
}

func (t *gptpay_transaction) needsClientConfirmation() (bool, error) {
	data, e := t.stripe.Retrieve()
	if e != nil {
		return false, e
	}
	if protocol_Truthy(data["error"]) || data["id"] != t.cp.PaymentIntentID {
		return false, protocol_NewError("payment_intent_state_unavailable")
	}
	status, ok := data["status"].(string)
	if !ok || !protocol_StringSet("requires_payment_method", "requires_confirmation", "requires_action", "processing", "requires_capture", "canceled", "succeeded")[status] {
		return false, protocol_NewError("payment_intent_state_unavailable")
	}
	t.observePayment(data)
	last, hasLast := data["last_payment_error"]
	pm, hasPM := data["payment_method"]
	// 同名 requires_payment_method 也可能来自既往拒绝，必须核对字段存在且明确为 null。
	initial := status == "requires_payment_method" && hasLast && last == nil && hasPM && pm == nil
	ready := (status == "requires_confirmation" || initial) && data["confirmation_method"] == "automatic" && last == nil && data["latest_charge"] == nil && data["next_action"] == nil
	outcome := "skipped"
	if ready {
		outcome = "ready"
	}
	t.observer.Emit("payment_confirm", protocol_Map{"phase": t.cp.Phase, "outcome": outcome, "intent_status": status, "payment_state": t.cp.Payment})
	return ready, nil
}

func gptpay_paymentPageIDKind(data protocol_Map) string {
	id := protocol_AsString(data["id"])
	switch {
	case strings.HasPrefix(id, "ppage_"):
		return "ppage"
	case strings.HasPrefix(id, "cs_"):
		return "cs"
	case id == "":
		return "missing"
	default:
		return "other"
	}
}

func gptpay_hasPaymentIntent(data protocol_Map) bool {
	switch raw := data["payment_intent"].(type) {
	case string:
		return strings.HasPrefix(raw, "pi_") && !strings.Contains(raw, "_secret_")
	default:
		id := protocol_AsString(protocol_AsMap(raw)["id"])
		return strings.HasPrefix(id, "pi_")
	}
}

func (t *gptpay_transaction) pollCustomPaymentAfterApprove(data protocol_Map) (protocol_Map, error) {
	var e error
	for i := 0; i < 10; i++ {
		state := stripe_Classify(data)
		if state == model_PaymentCharged || state == model_PaymentActionRequired || state == model_PaymentDeclined {
			return data, nil
		}
		if e = t.stripeHTTP.Pause(1.5); e != nil {
			return nil, e
		}
		data, e = t.stripe.CustomPage()
		if e != nil {
			return nil, e
		}
	}
	return data, nil
}
