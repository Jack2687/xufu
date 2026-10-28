// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	url "net/url"
	strings "strings"
	time "time"
)

func (t *gptpay_transaction) prepareCheckout(mode string, stripeRequired bool) (protocol_Map, error) {
	t.progress.emit(model_ProgressStageSession, model_ProgressRunning, "")
	if e := t.save("initializing"); e != nil {
		return nil, e
	}
	if e := t.platform.Homepage(); e != nil {
		return nil, e
	}
	if e := t.platform.Refresh(); e != nil && (protocol_FlowErrorCode(e) != "session_expired" || t.cp.AccessToken == "") {
		return nil, e
	}
	account, e := t.platform.Account()
	if protocol_FlowErrorCode(e) == "http_401" {
		if e = t.platform.Refresh(); e == nil {
			account, e = t.platform.Account()
		}
	}
	if e != nil {
		return nil, e
	}
	cp := t.cp
	ent := protocol_AsMap(account["entitlement"])
	baseline := protocol_Map{"plan_type": protocol_AsString(protocol_FirstTruthy(protocol_AsMap(account["account"])["plan_type"], ent["plan_type"], "")), "subscription_plan": protocol_AsString(ent["subscription_plan"]), "has_active_subscription": ent["has_active_subscription"], "cancels_at": protocol_AsString(ent["cancels_at"])}
	_, until, renewal, e := t.loadCurrentSubscription(baseline)
	if e != nil {
		return nil, e
	}
	cp.Metadata["subscription_baseline"] = baseline
	flow := gptpay_accountPaymentFlow(cp.Plan, baseline)
	t.emitAccountStatus(baseline, until, renewal, flow)
	if flow == "subscription_update" {
		cp.Metadata["payment_flow"] = "subscription_update"
		cp.Metadata["updated_plan"] = model_CheckoutPlan(cp.Plan)
		if e = t.save("account_checked"); e != nil {
			return nil, e
		}
		t.progress.emit(model_ProgressStageSession, model_ProgressSucceeded, "")
		t.progress.emit(model_ProgressStageCheckout, model_ProgressRunning, "")
		return protocol_Map{"payment_flow": "subscription_update"}, nil
	}
	if e = t.save("account_checked"); e != nil {
		return nil, e
	}
	if flow == "existing_subscription" {
		return nil, protocol_NewError("existing_subscription")
	}
	t.progress.emit(model_ProgressStageSession, model_ProgressSucceeded, "")
	t.progress.emit(model_ProgressStageCheckout, model_ProgressRunning, "")
	if stripeRequired {
		if e = t.initializeStripe(); e != nil {
			return nil, e
		}
		if e = t.save("stripe_ready"); e != nil {
			return nil, e
		}
	}
	if !stripeRequired && mode == "hosted" {
		// 只提前建立独立隧道；仍在 Sentinel 完成后发送唯一一次创建请求。
		if h, ok := t.http.(interface{ Preconnect(string) }); ok {
			h.Preconnect(chatgpt_PlatformBase + "/backend-api/payments/checkout")
		}
	}
	token, e := t.sentinel.Token("chatgpt_checkout")
	if e != nil {
		return nil, e
	}
	checkout, e := t.platform.Checkout(model_CheckoutPlan(cp.Plan), token, mode)
	if e != nil {
		return nil, e
	}
	cp.CheckoutID = protocol_AsString(protocol_FirstTruthy(checkout["checkout_session_id"], checkout["stripe_checkout_session_id"], ""))
	if !protocol_Matches(`(?:oaics_|cs_live_)[A-Za-z0-9]+`, cp.CheckoutID) {
		return nil, protocol_NewError("checkout_id_invalid")
	}
	prefix, route := "oaics_", "chatgpt"
	if strings.HasPrefix(cp.CheckoutID, "cs_live_") {
		prefix, route = "cs_live_", "stripe"
	}
	t.observer.Emit("checkout_route", protocol_Map{"checkout_prefix": prefix, "payment_route": route, "outcome": "selected"})
	cp.Entity = protocol_AsString(protocol_FirstTruthy(checkout["processor_entity"], "openai_llc"))
	if !protocol_Matches(`[a-z0-9_]{2,64}`, cp.Entity) {
		return nil, protocol_NewError("processor_entity_invalid")
	}
	actual, ok := checkout["checkout_ui_mode"].(string)
	if !ok || actual != "custom" && actual != "redirect" && actual != "hosted" {
		return nil, protocol_NewError("checkout_ui_mode_invalid")
	}
	if actual == "hosted" {
		actual = "redirect"
	}
	cp.Metadata["checkout_ui_mode"] = actual
	// Keep only an actual upstream link for the server's encrypted admin history.
	if raw := stripe_FindHostedURL(checkout, cp.CheckoutID); raw != "" {
		cp.Metadata["hosted_url"] = raw
	}
	if secret, ok := checkout["client_secret"].(string); ok && strings.HasPrefix(secret, cp.CheckoutID+"_secret_") && len(secret) > len(cp.CheckoutID+"_secret_") {
		cp.Metadata["checkout_client_secret"] = secret
	}
	if raw := protocol_AsString(checkout["confirm_return_url"]); raw != "" {
		if u, err := chatgpt_ValidateReturnURL(raw, cp); err == nil {
			cp.ReturnURL = u
		}
	}
	if v := checkout["publishable_key"]; v != nil {
		key, ok := v.(string)
		if !ok || !protocol_Matches(`pk_[A-Za-z0-9_]+`, key) {
			return nil, protocol_NewError("publishable_key_missing")
		}
		cp.PublishableKey = key
	}
	if e = t.save("checkout_created"); e != nil {
		return nil, e
	}
	return checkout, nil
}

func (t *gptpay_transaction) initializeStripe() error {
	// Hosted 链接补全可能发生在创建之后，不能覆盖 Checkout 返回的公钥。
	if t.cp.PublishableKey == "" {
		bootstrap, e := t.platform.Call("GET", "/backend-api/payments/stripe_client_bootstrap?account_id="+url.QueryEscape(t.cp.AccountID))
		if e != nil {
			return e
		}
		t.cp.PublishableKey = protocol_AsString(protocol_FirstTruthy(bootstrap["publishable_key"], bootstrap["publishableKey"]))
		if !strings.HasPrefix(t.cp.PublishableKey, "pk_") {
			return protocol_NewError("publishable_key_missing")
		}
	}
	return t.stripe.Initialize()
}

func (t *gptpay_transaction) checkoutURL(checkout protocol_Map, stripeReady bool) (string, error) {
	if t.cp.Metadata["checkout_ui_mode"] == "custom" {
		return chatgpt_PlatformBase + "/checkout/" + t.cp.Entity + "/" + t.cp.CheckoutID, nil
	}
	if raw := stripe_FindHostedURL(checkout, t.cp.CheckoutID); raw != "" {
		return raw, nil
	}
	if strings.HasPrefix(t.cp.CheckoutID, "cs_live_") {
		if e := t.ensurePublishableKey(checkout); e != nil {
			return "", e
		}
		return t.stripe.ExtractHostedURL(checkout)
	}
	if raw := stripe_FindHostedURL(checkout, t.cp.CheckoutID); raw != "" || stripeReady {
		return t.stripe.HostedCheckoutURL(raw)
	}
	if e := t.initializeStripe(); e != nil {
		return "", e
	}
	return t.stripe.HostedCheckoutURL("")
}

// standaloneHostedURL 尊重调用方的长链要求；上游可能返回 custom 模式的 cs_live 单。
// 只读取同一结算单的完整链接，不拼接 fragment，也不再次创建结算单。
func (t *gptpay_transaction) standaloneHostedURL(checkout protocol_Map) (string, error) {
	if !strings.HasPrefix(t.cp.CheckoutID, "cs_live_") {
		return "", protocol_NewError("checkout_id_invalid")
	}
	if raw := stripe_FindHostedURL(checkout, t.cp.CheckoutID); raw != "" {
		return raw, nil
	}
	if err := t.ensurePublishableKey(checkout); err != nil {
		return "", err
	}
	return t.stripe.ExtractHostedURL(checkout)
}

func (t *gptpay_transaction) ensurePublishableKey(checkout protocol_Map) error {
	if strings.HasPrefix(t.cp.PublishableKey, "pk_") {
		return nil
	}
	if key, ok := checkout["publishable_key"].(string); ok && protocol_Matches(`pk_[A-Za-z0-9_]+`, key) {
		t.cp.PublishableKey = key
		return nil
	}
	bootstrap, e := t.platform.Call("GET", "/backend-api/payments/stripe_client_bootstrap?account_id="+url.QueryEscape(t.cp.AccountID))
	if e != nil {
		return e
	}
	t.cp.PublishableKey = protocol_AsString(protocol_FirstTruthy(bootstrap["publishable_key"], bootstrap["publishableKey"]))
	if !strings.HasPrefix(t.cp.PublishableKey, "pk_") {
		return protocol_NewError("publishable_key_missing")
	}
	return nil
}

func gptpay_minor(v any) *int64 {
	if m, ok := v.(protocol_Map); ok {
		v = m["minorUnitsAmount"]
	}
	n, ok := protocol_Integer(v)
	if !ok || n < 0 {
		return nil
	}
	return &n
}

func gptpay_firstMinor(v ...any) *int64 {
	for _, x := range v {
		if n := gptpay_minor(x); n != nil {
			return n
		}
	}
	return nil
}

func gptpay_extractPrice(data protocol_Map, currency string) (*model_CheckoutPrice, error) {
	nodes := []protocol_Map{data}
	for _, k := range []string{"checkout_session", "checkout_snapshot", "checkout_state"} {
		for _, n := range append([]protocol_Map{}, nodes...) {
			if child, ok := n[k].(protocol_Map); ok {
				nodes = append(nodes, child)
			}
		}
	}
	for _, n := range nodes {
		for _, v := range []any{n["currency"], protocol_AsMap(n["billing_details"])["currency"]} {
			if v != nil {
				c, ok := v.(string)
				if !ok || strings.ToUpper(c) != currency {
					return nil, protocol_NewError("checkout_currency_mismatch")
				}
			}
		}
	}
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		totals, details, summary, invoice := protocol_AsMap(n["total"]), protocol_AsMap(n["total_details"]), protocol_AsMap(n["total_summary"]), protocol_AsMap(n["invoice"])
		subtotal := gptpay_firstMinor(n["amount_subtotal"], totals["subtotal"])
		total := gptpay_firstMinor(n["amount_total"], totals["total"], summary["due"], summary["total"], n["amount_due"], invoice["amount_due"], summary["amount_due"])
		if subtotal != nil || total != nil {
			return &model_CheckoutPrice{Currency: currency, SubtotalMinor: subtotal, TotalMinor: total, TaxMinor: gptpay_firstMinor(details["amount_tax"], totals["tax"]), DiscountMinor: gptpay_firstMinor(details["amount_discount"], totals["discount"])}, nil
		}
	}
	return nil, nil
}

func (t *gptpay_transaction) readCheckoutPrice(checkout protocol_Map, custom bool, url string) (price *model_CheckoutPrice, err error) {
	started := time.Now()
	t.observer.Emit("price", protocol_Map{"outcome": "started", "phase": t.cp.Phase})
	defer func() {
		outcome := "completed"
		if err != nil {
			outcome = "failed"
		}
		t.observer.Emit("price", protocol_Map{"outcome": outcome, "phase": t.cp.Phase, "error_code": protocol_FlowErrorCode(err), "duration_ms": time.Since(started).Seconds() * 1000})
	}()
	price, err = gptpay_extractPrice(checkout, t.cp.Currency)
	if err != nil || price != nil {
		return
	}
	if !custom {
		// 提链已读取过 init 时先使用同一响应，缺少金额才走完整页面查询。
		price, err = gptpay_extractPrice(t.stripe.ExtractedHostedSnapshot(), t.cp.Currency)
		if err != nil || price != nil {
			return
		}
	}
	var snapshots []protocol_Map
	if custom && strings.HasPrefix(t.cp.CheckoutID, "cs_live_") {
		var m protocol_Map
		m, err = t.stripe.CustomPriceSnapshot(checkout)
		snapshots = []protocol_Map{m}
	} else if custom {
		return nil, protocol_NewError("checkout_price_missing")
	} else {
		if err = t.ensurePublishableKey(checkout); err != nil {
			return nil, err
		}
		snapshots, err = t.stripe.HostedPriceSnapshots(url)
	}
	if err != nil {
		return nil, err
	}
	for _, m := range snapshots {
		price, err = gptpay_extractPrice(m, t.cp.Currency)
		if err != nil || price != nil {
			return
		}
	}
	return nil, protocol_NewError("checkout_price_missing")
}

func (t *gptpay_transaction) loadCurrentSubscription(baseline protocol_Map) (protocol_Map, string, string, error) {
	subs, e := t.platform.Subscriptions(false)
	if protocol_FlowErrorCode(e) == "subscription_not_found" {
		return protocol_Map{}, "", "unknown", nil
	}
	if e != nil {
		return nil, "", "", e
	}
	if pt := strings.TrimSpace(protocol_AsString(subs["plan_type"])); pt != "" {
		baseline["plan_type"] = pt
	}
	if sp := strings.TrimSpace(protocol_AsString(protocol_FirstTruthy(subs["subscription_plan"], subs["plan"]))); sp != "" {
		baseline["subscription_plan"] = sp
	}
	until := protocol_AsString(protocol_FirstTruthy(subs["active_until"], subs["current_period_end"], subs["expires_at"], subs["billing_period_end"]))
	pt := strings.ToLower(protocol_AsString(baseline["plan_type"]))
	if pt != "" && pt != "free" && (protocol_Truthy(until) || subs["will_renew"] == true) {
		baseline["has_active_subscription"] = true
	}
	return subs, until, chatgpt_RenewalState(subs), nil
}

func gptpay_accountPaymentFlow(plan string, baseline protocol_Map) string {
	if gptpay_plusToProliteUpgrade(plan, baseline) {
		return "subscription_update"
	}
	if baseline["has_active_subscription"] == true {
		return "existing_subscription"
	}
	return "checkout"
}

func (t *gptpay_transaction) emitAccountStatus(baseline protocol_Map, until, renewal, flow string) {
	planType := strings.ToLower(strings.TrimSpace(protocol_AsString(baseline["plan_type"])))
	if planType == "" {
		planType = "free"
	}
	if renewal == "" {
		renewal = "unknown"
	}
	fields := protocol_Map{"plan_type": planType, "renewal": renewal, "flow": flow, "outcome": "checked"}
	if ts := gptpay_rfc3339UTC(until); ts != "" {
		fields["active_until"] = ts
	}
	t.observer.Emit("account", fields)
}

func gptpay_rfc3339UTC(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return ""
}
