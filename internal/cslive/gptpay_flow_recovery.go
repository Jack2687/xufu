// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
)

func (t *gptpay_transaction) finishPayment() error {
	if e := t.confirmActivation(); e != nil {
		return e
	}
	if !t.cp.CancelRequested {
		return t.save("completed")
	}
	return t.disableRenewal()
}

func (t *gptpay_transaction) confirmActivation() error {
	cp := t.cp
	if e := t.save("activation"); e != nil {
		return e
	}
	if cp.ReturnURL != "" {
		u, e := chatgpt_ValidateReturnURL(cp.ReturnURL, cp)
		if e == nil {
			if _, e = t.http.Request("GET", u, protocol_RequestOptions{Headers: t.platform.Headers("")}); e != nil {
				return e
			}
		} else if protocol_FlowErrorCode(e) != "invalid_return_url" {
			return e
		}
	}
	for i := 0; i < 5; i++ {
		if i > 0 {
			if e := t.http.Pause(2); e != nil {
				return e
			}
		}
		data, e := t.platform.Subscriptions(false)
		if protocol_FlowErrorCode(e) == "http_401" {
			if e = t.platform.Refresh(); e != nil {
				return e
			}
			if e = t.save(""); e != nil {
				return e
			}
			data, e = t.platform.Subscriptions(false)
		}
		// 已扣款后的特定“尚无订阅”404属于到账延迟，继续有限轮询。
		if protocol_FlowErrorCode(e) == "subscription_not_found" && cp.Payment == "charged" {
			continue
		}
		if e != nil {
			return e
		}
		if chatgpt_SubscriptionActive(data, cp.Plan) {
			cp.Activation = "active"
			cp.Renewal = chatgpt_RenewalState(data)
			break
		}
	}
	if e := t.save(""); e != nil {
		return e
	}
	if cp.Activation != "active" {
		return protocol_NewError("activation_pending")
	}
	return nil
}

func (t *gptpay_transaction) disableRenewal() error {
	cp := t.cp
	t.progress.emit(model_ProgressStageCancellation, model_ProgressRunning, "")
	if e := t.platform.Refresh(); e != nil {
		return e
	}
	if e := t.save("cancellation"); e != nil {
		return e
	}
	data, e := t.platform.Subscriptions(true)
	if e != nil {
		return e
	}
	cp.Renewal = chatgpt_RenewalState(data)
	if cp.Renewal == "unknown" {
		return protocol_NewError("renewal_state_unknown")
	}
	if cp.Renewal == "off" {
		return t.save("completed")
	}
	for i := 0; i < 2; i++ {
		if e = t.save("cancellation_requested"); e != nil {
			return e
		}
		_, e = t.platform.Call("POST", "/backend-api/subscriptions/cancel", chatgpt_PlatformCallOptions{Body: protocol_O("account_id", cp.AccountID), Mode: "renewal"})
		if e == nil {
			break
		}
		original := e
		data, e = t.platform.Subscriptions(true)
		if e != nil {
			return e
		}
		cp.Renewal = chatgpt_RenewalState(data)
		if cp.Renewal == "off" {
			return t.save("completed")
		}
		if i > 0 || cp.Renewal != "on" || !protocol_StringSet("network_error", "http_408", "http_429", "http_500", "http_502", "http_503", "http_504")[protocol_FlowErrorCode(original)] {
			return original
		}
	}
	for i := 0; i < 5; i++ {
		if i > 0 {
			if e = t.http.Pause(2); e != nil {
				return e
			}
		}
		data, e = t.platform.Subscriptions(true)
		if e != nil {
			return e
		}
		cp.Renewal = chatgpt_RenewalState(data)
		if cp.Renewal == "off" {
			return t.save("completed")
		}
	}
	return protocol_NewError("cancellation_pending")
}

func (t *gptpay_transaction) recoverPayment() error {
	cp := t.cp
	if cp.Payment == "charged" && cp.ChargedPrice() == nil && cp.PaymentIntentID != "" && cp.ClientSecret != "" {
		// 已知扣款仍可补齐原单实收，失败只影响收尾，不重放付款或降低资金状态。
		data, err := t.stripe.Retrieve()
		if err != nil {
			return err
		}
		if data["id"] != cp.PaymentIntentID {
			return protocol_NewError("payment_intent_state_unavailable")
		}
		t.recordChargedPrice(data)
		if cp.ChargedPrice() != nil {
			if err = t.save(""); err != nil {
				return err
			}
		}
	}
	if protocol_AsString(cp.Metadata["payment_flow"]) == "subscription_update" {
		return t.recoverSubscriptionUpdate()
	}
	if cp.Payment == "charged" {
		t.progress.emit(model_ProgressStagePayment, model_ProgressSucceeded, "")
	} else {
		// 恢复中的 payment 仅表示核验原交易，不会重新提交付款。
		t.progress.emit(model_ProgressStagePayment, model_ProgressRunning, "")
	}
	if model_IsPreSubmitPhase(cp.Phase) {
		return protocol_NewError("payment_not_submitted")
	}
	custom := cp.Metadata["checkout_ui_mode"] == "custom"
	if !custom {
		t.stripe.HostedURL = protocol_AsString(cp.Metadata["hosted_url"])
	}
	if cp.Payment != "charged" {
		if cp.PaymentIntentID == "" && custom && !strings.HasPrefix(cp.CheckoutID, "cs_live_") {
			data, e := t.platform.Call("GET", "/backend-api/payments/checkout/"+cp.CheckoutID)
			if e != nil {
				return e
			}
			secret := protocol_AsString(protocol_FirstTruthy(data["client_secret"], protocol_AsMap(data["checkout_session"])["client_secret"]))
			if !protocol_Matches(`pi_[A-Za-z0-9]+_secret_[A-Za-z0-9]+`, secret) {
				return protocol_NewError("payment_pending")
			}
			cp.ClientSecret = secret
			cp.PaymentIntentID = strings.Split(secret, "_secret_")[0]
		}
		data, e := t.stripe.Retrieve()
		if e != nil {
			return e
		}
		if cp.PaymentIntentID != "" && cp.ClientSecret != "" && data["id"] != cp.PaymentIntentID {
			return protocol_NewError("payment_intent_state_unavailable")
		}
		t.observePayment(data)
		// 仅恢复 Custom cs_live 原单的待批准步骤，不重发付款确认。
		// 使用最新查询状态；审批方法会在写请求前再次持久化检查点。
		submission := protocol_AsMap(data["submission_attempt"])
		if custom && strings.HasPrefix(cp.CheckoutID, "cs_live_") && cp.Payment == "unknown" && protocol_FirstTruthy(submission["state"], submission["status"]) == "requires_approval" {
			data, e = t.approveCustomCheckout(data)
			if e != nil {
				return e
			}
			t.observePayment(data)
		}
		if e = t.save("payment_result"); e != nil {
			return e
		}
	}
	if cp.Payment == "charged" {
		return t.finishPayment()
	}
	// 已确认未扣款和需验证不是处理中；仅修正错误分类，不重发交易。
	switch model_PaymentState(cp.Payment) {
	case model_PaymentDeclined:
		return protocol_NewError("not_charged")
	case model_PaymentActionRequired:
		return protocol_NewError("action_required")
	case model_PaymentAuthorized:
		return protocol_NewError("authorized")
	}
	return protocol_NewError("payment_pending")
}

func (t *gptpay_transaction) cancelRenewalOnly() (result model_Result) {
	defer t.close()
	defer func() {
		if recover() != nil {
			result = t.complete(protocol_NewError("protocol_error"))
		}
	}()
	return t.complete(t.runCancelRenewalOnly())
}

func (t *gptpay_transaction) runCancelRenewalOnly() error {
	t.progress.emit(model_ProgressStageSession, model_ProgressRunning, "")
	if e := t.save("initializing"); e != nil {
		return e
	}
	if t.cp.AccountID == "" || t.cp.Email == "" {
		if e := t.platform.Refresh(); e != nil {
			return e
		}
	}
	if e := t.platform.Homepage(); e != nil {
		return e
	}
	account, e := t.platform.Account()
	if protocol_FlowErrorCode(e) == "http_401" {
		if e = t.platform.Refresh(); e == nil {
			account, e = t.platform.Account()
		}
	}
	if e != nil {
		return e
	}
	ent := protocol_AsMap(account["entitlement"])
	if ent["has_active_subscription"] != true {
		return protocol_NewError("subscription_not_found")
	}
	t.cp.Activation = "active"
	t.progress.emit(model_ProgressStageSession, model_ProgressSucceeded, "")
	t.progress.emit(model_ProgressStageCancellation, model_ProgressRunning, "")
	return t.disableRenewal()
}
