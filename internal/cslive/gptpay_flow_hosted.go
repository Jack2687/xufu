// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	errors "errors"
	url "net/url"
	strings "strings"
)

func (t *gptpay_transaction) runHostedFlow(card model_Card, billing model_Billing, info protocol_Map) error {
	cp := t.cp
	cp.StripeVersion = stripe_HostedVersion
	cp.Metadata["hosted_url"] = t.stripe.HostedURL
	form, e := t.stripe.HostedForm(card, billing, info)
	if e != nil {
		return e
	}
	cp.ConfirmBody = form
	if e = t.boundary("hosted_confirm"); e != nil {
		return e
	}
	data, e := t.stripe.Call("POST", "/payment_pages/"+cp.CheckoutID+"/confirm", stripe_StripeCallOptions{Form: form, Hosted: true, AllowError: true})
	if e != nil {
		return e
	}
	data, e = t.pollHostedPayment(data)
	if e != nil {
		return e
	}
	t.observePayment(data)
	// 保存扣款快照前记录回跳地址，以便恢复辅助流程。
	landing, callback, state := t.selectHostedLanding(data)
	chargedSaved := cp.Payment == "charged"
	if chargedSaved {
		// 明确扣款后先保存可恢复的资金结果，再发辅助请求。
		if e = t.save("payment_result"); e != nil {
			return e
		}
	}
	if e = t.snapshotHostedBilling(billing); e != nil {
		return e
	}
	data, e = t.approveHostedPayment(data, landing, callback, state)
	if e != nil {
		return e
	}
	if chargedSaved {
		return nil
	}
	return t.save("payment_result")
}

func (t *gptpay_transaction) absorbHostedIntent(data protocol_Map) {
	raw := data["payment_intent"]
	if id := protocol_AsString(raw); strings.HasPrefix(id, "pi_") && !strings.Contains(id, "_secret_") {
		t.cp.PaymentIntentID = id
		return
	}
	pi := protocol_AsMap(raw)
	if id := protocol_AsString(pi["id"]); strings.HasPrefix(id, "pi_") {
		t.cp.PaymentIntentID = id
	}
	if secret := protocol_AsString(pi["client_secret"]); strings.Contains(secret, "_secret_") {
		t.cp.ClientSecret = secret
	}
}

func (t *gptpay_transaction) pollHostedPayment(data protocol_Map) (protocol_Map, error) {
	var e error
	for i := 0; i < 3; i++ {
		state := stripe_Classify(data)
		if state == model_PaymentCharged || state == model_PaymentActionRequired || state == model_PaymentDeclined {
			break
		}
		if e = t.stripeHTTP.Pause(1.2); e != nil {
			return nil, e
		}
		data, e = t.stripe.Page()
		if e != nil {
			return nil, e
		}
	}
	return data, nil
}

// 账单快照仅用于辅助同步；协议错误不能抹掉已经取得的资金状态。
func (t *gptpay_transaction) snapshotHostedBilling(billing model_Billing) error {
	_, e := t.platform.Call("POST", "/backend-api/payments/checkout/snapshot", chatgpt_PlatformCallOptions{Body: protocol_O("snapshot", protocol_O("billing_address", protocol_O("name", billing.Name, "address", billing.Address())))})
	if e != nil {
		var pe *protocol_ProtocolError
		if !errors.As(e, &pe) {
			return e
		}
	}
	return nil
}

func (t *gptpay_transaction) selectHostedLanding(data protocol_Map) (string, bool, any) {
	cp := t.cp
	submission := protocol_AsMap(data["submission_attempt"])
	state := protocol_FirstTruthy(submission["state"], submission["status"])
	cp.ReturnURL = chatgpt_PlatformBase + "/checkout/verify?stripe_session_id=" + url.QueryEscape(cp.CheckoutID) + "&processor_entity=" + url.QueryEscape(cp.Entity) + "&plan_type=" + model_HostedPlanType(cp.Plan)
	landing := cp.ReturnURL
	callback := false
	completion := protocol_AsMap(data["after_completion"])
	redirect := protocol_AsMap(completion["redirect"])
	for _, v := range []any{data["success_url"], data["return_url"], data["url"], redirect["url"], completion["url"]} {
		candidate := protocol_AsString(v)
		u, e := url.Parse(candidate)
		if e != nil {
			continue
		}
		q := u.Query()["stripe_session_id"]
		if u.Scheme == "https" && u.Host == "chatgpt.com" && u.User == nil && (len(q) == 1 && q[0] == cp.CheckoutID || u.Path == "/checkout/"+cp.Entity+"/"+cp.CheckoutID) {
			landing = candidate
			callback = true
			break
		}
	}
	return landing, callback, state
}

func (t *gptpay_transaction) approveHostedPayment(data protocol_Map, landing string, callback bool, state any) (protocol_Map, error) {
	cp := t.cp
	if !protocol_StringSet("charged", "action_required", "not_charged")[cp.Payment] && (callback || state == "requires_approval" || state == "requires_action") {
		path := "/backend-api/payments/checkout/approve"
		token, e := t.sentinel.Token("checkout_session_approval")
		if e != nil {
			return nil, e
		}
		headers, e := t.platform.ChallengeHeaders(path, token, landing, true)
		if e != nil {
			return nil, e
		}
		if e = t.save("hosted_approve"); e != nil {
			return nil, e
		}
		reply, e := t.platform.Call("POST", path, chatgpt_PlatformCallOptions{Body: protocol_O("checkout_session_id", cp.CheckoutID, "processor_entity", cp.Entity), Headers: headers})
		if e != nil {
			return nil, e
		}
		if protocol_FirstTruthy(reply["result"], reply["status"], reply["state"]) == "blocked" {
			return nil, protocol_NewError("approval_blocked")
		}
		data, e = t.stripe.Page()
		if e != nil {
			return nil, e
		}
		t.observePayment(data)
	}
	return data, nil
}
