// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	json "encoding/json"
	math "math"
	strings "strings"
	time "time"
)

func gptpay_plusToProliteUpgrade(plan string, baseline protocol_Map) bool {
	if plan != "pro5x" || baseline["has_active_subscription"] != true {
		return false
	}
	return model_MatchesSubscriptionType("plus", protocol_AsString(baseline["plan_type"])) || model_MatchesSubscriptionType("plus", protocol_AsString(baseline["subscription_plan"]))
}

func (t *gptpay_transaction) runSubscriptionUpdate(card *model_Card, billing model_Billing, expected *int64, approval model_PaymentApproval) error {
	if card == nil {
		if e := t.requireSavedPaymentMethod(); e != nil {
			return e
		}
	}
	price, e := t.readUpgradePrice()
	if e != nil {
		return e
	}
	if price == nil || price.TotalMinor == nil {
		return protocol_NewError("checkout_price_missing")
	}
	if e = t.pricing(*price.TotalMinor, price.Currency, expected, model_PriceSourceSubscriptionProration); e != nil {
		return e
	}
	if e = t.save("upgrade_priced"); e != nil {
		return e
	}
	t.progress.emit(model_ProgressStageCheckout, model_ProgressSucceeded, "")
	if card == nil {
		return nil
	}
	t.progress.emit(model_ProgressStagePayment, model_ProgressRunning, "")
	if approval != nil {
		if e = t.ctx.Err(); e != nil {
			return e
		}
		ok, e := gptpay_callApproval(approval, *t.cp.Amount, t.cp.Currency)
		if e != nil {
			return protocol_NewError("payment_approval_failed")
		}
		if !ok {
			return protocol_NewError("payment_cancelled")
		}
		if e = t.ctx.Err(); e != nil {
			return e
		}
	}
	// 报价或金额审批拒绝不得改变订阅续费状态。
	if e = t.ensurePlusRenewal(); e != nil {
		return e
	}
	pmID, e := t.bindUpgradePaymentMethod(*card, billing)
	if e != nil {
		return e
	}
	if e = t.boundary("subscription_update"); e != nil {
		return e
	}
	data, e := t.platform.UpdateSubscription(protocol_AsString(t.cp.Metadata["updated_plan"]), pmID, "")
	if e != nil {
		if protocol_FlowErrorCode(e) == "http_400" {
			t.setPaymentState(model_PaymentDeclined)
			if se := t.save("payment_result"); se != nil {
				return se
			}
			return protocol_NewError("not_charged")
		}
		return e
	}
	state, pi := gptpay_interpretSubscriptionUpdate(data)
	var received protocol_Map
	var intentErr error
	if pi != "" {
		t.cp.PaymentIntentID = pi
	}
	if secret := gptpay_upgradeClientSecret(data); secret != "" {
		t.cp.ClientSecret = secret
	}
	if t.cp.PaymentIntentID != "" && t.cp.ClientSecret != "" {
		if ver := t.stripe.PortalAPIVersion(); ver != "" {
			t.cp.StripeVersion = ver
		}
		if retrieved, err := t.stripe.Retrieve(); err != nil {
			intentErr = err
		} else if retrieved["id"] != t.cp.PaymentIntentID || protocol_Truthy(retrieved["error"]) {
			intentErr = protocol_NewError("payment_intent_state_unavailable")
		} else {
			received = retrieved
			if classified := stripe_Classify(retrieved); classified != model_PaymentUnknown {
				state = classified
			}
			errData := protocol_AsMap(protocol_FirstTruthy(retrieved["last_payment_error"], retrieved["error"]))
			t.observer.Emit("payment_error", protocol_Map{"service": "stripe", "path": "/v1/payment_intents", "provider_code": protocol_FirstTruthy(retrieved["status"], errData["code"]), "decline_code": protocol_FirstTruthy(errData["decline_code"], errData["code"], data["payment_method_failure_reason"])})
		}
	} else if state == model_PaymentDeclined {
		errData := protocol_AsMap(protocol_FirstTruthy(data["last_payment_error"], data["error"]))
		t.observer.Emit("payment_error", protocol_Map{"service": "chatgpt", "path": "/backend-api/subscriptions/update", "provider_code": protocol_FirstTruthy(data["status"], errData["code"]), "decline_code": protocol_FirstTruthy(gptpay_upgradeFailureCode(data), errData["decline_code"], errData["code"])})
	}
	if state == "" {
		state = model_PaymentPending
	}
	t.setPaymentState(state)
	t.recordChargedPrice(received)
	if e = t.save("payment_result"); e != nil {
		return e
	}
	if intentErr != nil {
		return intentErr
	}
	if state == model_PaymentDeclined && pi != "" {
		if _, e = t.platform.CancelPendingUpdate(pi); e != nil && protocol_FlowErrorCode(e) != "http_404" {
			t.observer.Emit("phase", protocol_Map{"phase": t.cp.Phase, "outcome": "failed", "error_code": protocol_FlowErrorCode(e)})
		}
	}
	if t.cp.Payment == "charged" {
		return t.finishPayment()
	}
	return protocol_NewError(string(state))
}

func (t *gptpay_transaction) ensurePlusRenewal() error {
	data, e := t.platform.Subscriptions(false)
	if e != nil {
		return e
	}
	if chatgpt_RenewalState(data) != "off" {
		return nil
	}
	if e = t.save(""); e != nil {
		return e
	}
	_, e = t.platform.RenewSubscription()
	return e
}

func (t *gptpay_transaction) requireSavedPaymentMethod() error {
	_, e := t.bindUpgradePaymentMethod(model_Card{}, model_Billing{})
	return e
}

func (t *gptpay_transaction) bindUpgradePaymentMethod(card model_Card, billing model_Billing) (string, error) {
	data, e := t.platform.PaymentMethods()
	if e != nil {
		return "", e
	}
	methods, _ := data["payment_methods"].([]any)
	defaultID := protocol_AsString(data["default_payment_method_id"])
	last4 := ""
	if n := card.Number; len(n) >= 4 {
		last4 = n[len(n)-4:]
	}
	if last4 != "" {
		// 传入完整卡时重新走 portal 挂载，让 Stripe.js 指纹和 Payment Element 会话落在新 pm_ 上。
		pmID, asDefault, e := t.attachUpgradeCard(card, billing)
		if e != nil {
			return "", e
		}
		t.cp.Metadata["payment_method_id"] = pmID
		if asDefault {
			return "", nil
		}
		return pmID, nil
	}
	if defaultID == "" || len(methods) == 0 {
		return "", protocol_NewError("payment_method_missing")
	}
	t.cp.Metadata["payment_method_id"] = defaultID
	return "", nil
}

func (t *gptpay_transaction) attachUpgradeCard(card model_Card, billing model_Billing) (string, bool, error) {
	raw, e := t.platform.CustomerPortal()
	if e != nil {
		return "", false, e
	}
	if e = t.stripe.LoadPortal(raw); e != nil {
		return "", false, e
	}
	if e = t.initializeStripe(); e != nil {
		return "", false, e
	}
	seti, secret, e := t.stripe.CreatePortalSetupIntent()
	if e != nil {
		return "", false, e
	}
	t.cp.Metadata["setup_intent_id"] = seti
	if e = t.stripe.BindPortalElements(secret); e != nil {
		return "", false, e
	}
	token, e := t.stripe.PortalConfirmationToken(card, billing)
	if e != nil {
		return "", false, e
	}
	pmID, e := t.stripe.ConfirmPortalSetupIntent(seti, secret, "", token)
	if e != nil {
		return "", false, e
	}
	if e = t.stripe.SetPortalDefaultPaymentMethod(pmID); e != nil {
		t.observer.Emit("phase", protocol_Map{"phase": t.cp.Phase, "outcome": "failed", "error_code": protocol_FlowErrorCode(e)})
		return pmID, false, nil
	}
	// Stripe 默认不等于 ChatGPT 已保存卡；不登记的话 update 仍可能扣旧 pm_。
	if _, e = t.platform.AttachPaymentMethod(pmID); e != nil {
		t.observer.Emit("phase", protocol_Map{"phase": t.cp.Phase, "outcome": "failed", "error_code": protocol_FlowErrorCode(e)})
		return pmID, false, nil
	}
	return pmID, true, nil
}

func (t *gptpay_transaction) readUpgradePrice() (*model_CheckoutPrice, error) {
	subs, e := t.platform.Subscriptions(false)
	if e != nil {
		return nil, e
	}
	cfg, e := t.platform.PricingConfig(t.cp.Country)
	if e != nil {
		return nil, e
	}
	return gptpay_estimateUpgradePrice(cfg, subs, t.cp.Currency, time.Now())
}

func gptpay_interpretSubscriptionUpdate(data protocol_Map) (model_PaymentState, string) {
	pi := protocol_AsString(protocol_FirstTruthy(data["pending_update_payment_intent_id"], data["payment_intent_id"]))
	status := strings.ToLower(protocol_AsString(data["status"]))
	reason := gptpay_upgradeFailureCode(data)
	decline := reason
	if len(protocol_AsMap(data["next_action"])) > 0 || len(protocol_AsMap(protocol_AsMap(data["payment_intent"])["next_action"])) > 0 {
		return model_PaymentActionRequired, pi
	}
	switch {
	case status == "requires_action" || status == "action_required" || decline == "authentication_required" || strings.Contains(reason, "authenticat"):
		return model_PaymentActionRequired, pi
	case status == "payment_method_failed" || strings.Contains(reason, "declined") || decline != "":
		return model_PaymentDeclined, pi
	case protocol_StringSet("success", "ok", "completed", "complete", "updated", "paid", "charged", "active", "succeeded")[status]:
		return model_PaymentCharged, pi
	default:
		return "", pi
	}
}

func gptpay_upgradeFailureCode(data protocol_Map) string {
	nested := []protocol_Map{protocol_AsMap(data["payment_method_failure_reason"]), protocol_AsMap(data["last_payment_error"]), protocol_AsMap(data["error"]), protocol_AsMap(data["payment_intent"])}
	values := []any{data["payment_method_failure_reason"], data["decline_code"], data["failure_code"]}
	for _, node := range nested {
		values = append(values, node["decline_code"], node["code"], node["reason"], node["failure_code"])
	}
	for _, v := range values {
		if _, isMap := v.(protocol_Map); isMap {
			continue
		}
		s := strings.ToLower(strings.TrimSpace(protocol_AsString(v)))
		if s == "" || strings.HasPrefix(s, "map[") {
			continue
		}
		return s
	}
	return ""
}

func gptpay_upgradeClientSecret(data protocol_Map) string {
	for _, v := range []any{data["client_secret"], data["pending_update_client_secret"], protocol_AsMap(data["payment_intent"])["client_secret"], protocol_AsMap(data["pending_update"])["client_secret"], protocol_AsMap(data["stripe_payment_intent"])["client_secret"]} {
		s := protocol_AsString(v)
		if strings.Contains(s, "_secret_") {
			return s
		}
	}
	return ""
}

func gptpay_estimateUpgradePrice(cfg, subs protocol_Map, currency string, now time.Time) (*model_CheckoutPrice, error) {
	currency = strings.ToUpper(currency)
	node := protocol_AsMap(cfg["currency_config"])
	if code := strings.ToUpper(protocol_AsString(protocol_FirstTruthy(cfg["symbol_code"], node["symbol_code"]))); code != "" && code != currency {
		return nil, protocol_NewError("checkout_currency_mismatch")
	}
	exp := 2
	if n, ok := protocol_Integer(protocol_FirstTruthy(cfg["minor_unit_exponent"], node["minor_unit_exponent"])); ok {
		if n < 0 || n > 4 {
			return nil, protocol_NewError("checkout_price_missing")
		}
		exp = int(n)
	}
	prolite, ok := gptpay_monthAmountMinor(node["prolite"], exp)
	if !ok {
		return nil, protocol_NewError("checkout_price_missing")
	}
	plus, _ := gptpay_monthAmountMinor(node["plus"], exp)
	start := gptpay_parseTime(subs["active_start"])
	until := gptpay_parseTime(protocol_FirstTruthy(subs["active_until"], subs["current_period_end"], subs["expires_at"], subs["billing_period_end"]))
	credit := gptpay_prorateCredit(plus, start, until, now)
	due := prolite - credit
	if due < 0 {
		due = 0
	}
	return &model_CheckoutPrice{Currency: currency, SubtotalMinor: &prolite, TotalMinor: &due, DiscountMinor: &credit}, nil
}

func gptpay_monthAmountMinor(plan any, exp int) (int64, bool) {
	month := protocol_AsMap(protocol_AsMap(plan)["month"])
	override := protocol_AsMap(month["psp_override"])
	return gptpay_toMinor(protocol_FirstTruthy(override["amount"], month["amount"]), exp)
}

func gptpay_toMinor(v any, exp int) (int64, bool) {
	f, ok := gptpay_jsonFloat(v)
	if !ok || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	n := math.Round(f * math.Pow10(exp))
	// MaxInt64 转成 float64 会向上舍入为 2^63，等于此边界也会溢出。
	if n >= float64(math.MaxInt64) {
		return 0, false
	}
	return int64(n), true
}

func gptpay_jsonFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	}
	if n, ok := protocol_Integer(v); ok {
		return float64(n), true
	}
	return 0, false
}

func gptpay_parseTime(v any) time.Time {
	s := protocol_AsString(v)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func gptpay_prorateCredit(plusMinor int64, start, until, now time.Time) int64 {
	if plusMinor <= 0 || until.IsZero() {
		return 0
	}
	if now.After(until) || now.Equal(until) {
		return 0
	}
	if start.IsZero() || !until.After(start) {
		return 0
	}
	remain := until.Sub(now)
	period := until.Sub(start)
	if remain >= period {
		return plusMinor
	}
	return int64(math.Round(float64(plusMinor) * float64(remain) / float64(period)))
}

func (t *gptpay_transaction) recoverSubscriptionUpdate() error {
	cp := t.cp
	if cp.Payment == "charged" {
		t.progress.emit(model_ProgressStagePayment, model_ProgressSucceeded, "")
		return t.finishPayment()
	}
	t.progress.emit(model_ProgressStagePayment, model_ProgressRunning, "")
	if model_IsPreSubmitPhase(cp.Phase) {
		return protocol_NewError("payment_not_submitted")
	}
	if cp.PaymentIntentID != "" && cp.ClientSecret != "" {
		data, err := t.stripe.Retrieve()
		if err != nil {
			return err
		}
		if data["id"] != cp.PaymentIntentID || protocol_Truthy(data["error"]) {
			return protocol_NewError("payment_intent_state_unavailable")
		}
		t.observePayment(data)
		if err = t.save("payment_result"); err != nil {
			return err
		}
		if cp.Payment == "charged" {
			return t.finishPayment()
		}
		if cp.Payment == "not_charged" {
			return protocol_NewError("not_charged")
		}
		return protocol_NewError("payment_pending")
	}
	if e := t.platform.Homepage(); e != nil {
		return e
	}
	_, e := t.platform.Subscriptions(false)
	if protocol_FlowErrorCode(e) == "http_401" {
		if e = t.platform.Refresh(); e != nil {
			return e
		}
		if e = t.save(""); e != nil {
			return e
		}
		_, e = t.platform.Subscriptions(false)
	}
	if e != nil {
		return e
	}
	// 账户套餐状态不证明原单已实扣，缺少原单证据时保留未确认资金。
	if e = t.save("payment_result"); e != nil {
		return e
	}
	if cp.Payment == "not_charged" {
		return protocol_NewError("not_charged")
	}
	return protocol_NewError("payment_pending")
}
