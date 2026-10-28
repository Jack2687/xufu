// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	url "net/url"
	strconv "strconv"
	strings "strings"
	time "time"
)

// LoadHostedPublishableKey 现成长链付款不再访问 ChatGPT bootstrap。
// 优先解码 hosted fragment；合成测试链没有有效 fragment 时再读页面字段。
func (s *stripe_Stripe) LoadHostedPublishableKey(raw string) error {
	if strings.HasPrefix(s.CP.PublishableKey, "pk_") {
		return nil
	}
	if err := s.ValidateHostedURL(raw); err != nil {
		return err
	}
	s.HostedURL = raw
	if key := stripe_ExtractHostedPublishableKeyFromURL(raw); protocol_Matches(`pk_[A-Za-z0-9_]+`, key) {
		s.CP.PublishableKey = key
		return nil
	}
	response, err := s.HTTP.Request("GET", strings.Split(raw, "#")[0], protocol_RequestOptions{Headers: s.Profile.Headers()})
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return &protocol_ProtocolError{Code: "hosted_page_unavailable"}
	}
	key := stripe_ExtractHostedPublishableKey(string(response.Content))
	if !protocol_Matches(`pk_[A-Za-z0-9_]+`, key) {
		return protocol_NewError("publishable_key_missing")
	}
	s.CP.PublishableKey = key
	return nil
}

func (s *stripe_Stripe) HostedCheckoutURL(raw string) (string, error) {
	if raw != "" {
		if err := s.ValidateHostedURL(raw); err != nil {
			return "", err
		}
		return raw, nil
	}
	_, err := s.initializeHosted("")
	if err != nil {
		return "", err
	}
	return s.HostedURL, nil
}

// ExtractHostedURL 只从创建响应或 Stripe init 读取完整长链。
// 路径-only URL 仅作 init 的 Origin/Referer 上下文，不能拼 fragment，也不能当作结果返回。
func (s *stripe_Stripe) ExtractHostedURL(checkout protocol_Map) (string, error) {
	if raw := stripe_FindHostedURL(checkout, s.CP.CheckoutID); raw != "" {
		s.HostedURL = raw
		return raw, nil
	}
	if !strings.HasPrefix(s.CP.CheckoutID, "cs_live_") {
		return "", protocol_NewError("checkout_id_invalid")
	}
	if !strings.HasPrefix(s.CP.PublishableKey, "pk_") {
		return "", protocol_NewError("publishable_key_missing")
	}
	s.HostedURL = "https://checkout.stripe.com/c/pay/" + s.CP.CheckoutID
	if err := s.ValidateHostedURL(s.HostedURL, false); err != nil {
		return "", err
	}
	init, err := s.Call("POST", "/payment_pages/"+s.CP.CheckoutID+"/init", stripe_StripeCallOptions{Hosted: true, Form: s.hostedInitForm()})
	if err != nil {
		return "", err
	}
	s.rememberHostedEid(init)
	raw := stripe_FindHostedURL(init, s.CP.CheckoutID)
	if raw == "" {
		return "", protocol_NewError("hosted_url_missing")
	}
	s.HostedURL = raw
	s.extractedHostedInit = init
	return raw, nil
}

// ExtractedHostedSnapshot 复用本次提链的 init 响应，不跨交易缓存价格。
func (s *stripe_Stripe) ExtractedHostedSnapshot() protocol_Map {
	return s.extractedHostedInit
}

func (s *stripe_Stripe) initializeHosted(raw string) (protocol_Map, error) {
	s.HostedURL = raw
	if raw == "" {
		s.HostedURL = "https://checkout.stripe.com/c/pay/" + s.CP.CheckoutID
	}
	if err := s.ValidateHostedURL(s.HostedURL, raw != ""); err != nil {
		return nil, err
	}
	init, err := s.Call("POST", "/payment_pages/"+s.CP.CheckoutID+"/init", stripe_StripeCallOptions{Hosted: true, Form: s.hostedInitForm()})
	if err != nil {
		return nil, err
	}
	s.rememberHostedEid(init)
	if raw == "" {
		s.HostedURL = stripe_FindHostedURL(init, s.CP.CheckoutID)
		if s.HostedURL == "" {
			return nil, &protocol_ProtocolError{Code: "hosted_url_missing"}
		}
		if err := s.ValidateHostedURL(s.HostedURL); err != nil {
			return nil, err
		}
	}
	s.hostedSnapshot = []protocol_Map{init}
	return init, nil
}

func (s *stripe_Stripe) HostedPriceSnapshots(raw string) ([]protocol_Map, error) {
	if s.hostedSnapshot == nil {
		if _, err := s.initializeHosted(raw); err != nil {
			return nil, err
		}
	}
	return s.hostedSnapshot, nil
}

func (s *stripe_Stripe) CustomCheckoutForm(init protocol_Map, paymentMethod string) (protocol_Fields, error) {
	if !stripe_validStripeJSVersion(s.JSVersion) {
		return nil, protocol_NewError("stripe_build_missing")
	}
	cp := s.CP
	if !strings.HasPrefix(cp.CheckoutID, "cs_live_") {
		return nil, protocol_NewError("checkout_id_invalid")
	}
	if paymentMethod == "" && cp.ConfirmationToken == "" {
		return nil, protocol_NewError("confirmation_token_missing")
	}
	if cp.ReturnURL == "" {
		return nil, protocol_NewError("invalid_return_url")
	}
	checksum := protocol_AsString(init["init_checksum"])
	if checksum == "" {
		return nil, protocol_NewError("init_checksum_missing")
	}
	var expected any
	if due := stripe_HostedAmount(init); due != nil {
		expected = *due
	} else if cp.Amount != nil {
		expected = *cp.Amount
	}
	selectionFlow := "merchant_specified"
	if init["automatic_payment_method_types"] == true {
		selectionFlow = "automatic"
	}
	attribution := protocol_O("client_session_id", s.ClientID, "checkout_session_id", cp.CheckoutID, "merchant_integration_source", "checkout", "merchant_integration_version", "custom", "merchant_integration_subtype", "payment-element", "payment_intent_creation_flow", "deferred", "payment_method_selection_flow", selectionFlow, "elements_session_id", s.ElementsID)
	if configID := protocol_AsString(init["config_id"]); configID != "" {
		attribution.Set("checkout_config_id", configID)
	}
	if s.ConfigID != "" {
		attribution.Set("elements_session_config_id", s.ConfigID)
	}
	sessionClient := protocol_O("elements_init_source", "custom_checkout", "referrer_host", "chatgpt.com", "stripe_js_id", s.ClientID, "locale", "en-US", "is_aggregation_expected", "false", "session_id", s.ElementsID)
	if betas := stripe_customClientBetas(init); len(betas) > 0 {
		sessionClient.Set("client_betas", betas)
	}
	form := protocol_O("expected_payment_method_type", "card", "expected_amount", expected, "key", cp.PublishableKey, "_stripe_version", stripe_checkoutPageVersion(init, cp.StripeVersion), "return_url", cp.ReturnURL, "init_checksum", checksum, "version", s.JSVersion, "elements_session_client", sessionClient, "elements_options_client", protocol_O("saved_payment_method", protocol_O("enable_save", "auto", "enable_redisplay", "auto")), "client_attribution_metadata", attribution)
	if stripe_consentRequired(init) {
		form.Set("consent", protocol_O("terms_of_service", "accepted"))
	}
	form.Update(s.IDs())
	// Payment Page confirm 不接受顶层 radar_options；会话信息已由
	// Token 的 payment_method_data 携带，不能重复放入此接口。
	checksumID := protocol_AsString(init["id"])
	if paymentMethod != "" {
		form.Set("payment_method", paymentMethod)
		checksumID = paymentMethod
	} else {
		form.Set("confirmation_token", cp.ConfirmationToken)
		if !strings.HasPrefix(checksumID, "ppage_") {
			return nil, protocol_NewError("payment_page_id_missing")
		}
	}
	jsChecksum, err := stripe_ComputeIDChecksum(checksumID)
	if err != nil {
		return nil, err
	}
	rvTS, err := stripe_ComputeRVTimestamp(s.Build["rv_ts"], s.Build["rv"], s.Build["sv"])
	if err != nil {
		return nil, err
	}
	form.Set("js_checksum", jsChecksum)
	form.Set("rv_timestamp", rvTS)
	return form, nil
}

func (s *stripe_Stripe) CustomPriceSnapshot(checkout protocol_Map) (protocol_Map, error) {
	key, ok := protocol_FirstTruthy(checkout["publishable_key"], s.CP.PublishableKey).(string)
	if !ok || !strings.HasPrefix(key, "pk_") {
		return nil, &protocol_ProtocolError{Code: "publishable_key_missing"}
	}
	// 分流只看创建响应的 checkout_session_id / checkout_ui_mode。
	// payment_pages/init 的 session_id 经常是 Elements 会话，不是 cs_live_；
	// create 的 client_secret 也不是这条 Custom 路径的入参。
	return s.Call("POST", "/payment_pages/"+s.CP.CheckoutID+"/init", stripe_StripeCallOptions{Form: protocol_O("key", key, "_stripe_version", s.CP.StripeVersion, "browser_locale", "en-US", "browser_timezone", s.CP.Timezone, "elements_session_client", protocol_O("stripe_js_id", s.ClientID, "elements_init_source", "custom_checkout", "referrer_host", "chatgpt.com", "locale", "en-US"), "elements_options_client", protocol_O("saved_payment_method", protocol_O("enable_save", "auto", "enable_redisplay", "auto")))})
}

func (s *stripe_Stripe) BindCheckoutElements(_ protocol_Map) error {
	query := url.Values{}
	query.Set("key", s.CP.PublishableKey)
	query.Set("_stripe_version", s.CP.StripeVersion)
	// Basil 只接受 payment_intent / setup_intent / deferred_intent。
	// Checkout Session secret 不是 PI/SI：Stripe.js 此时 type=deferred_intent，
	// 且不把 cs_…_secret_ 放进 client_secret，改为带 deferred_intent 明细。
	query.Set("type", "deferred_intent")
	query.Set("checkout_session_id", s.CP.CheckoutID)
	query.Set("elements_init_source", "custom_checkout")
	query.Set("referrer_host", "chatgpt.com")
	query.Set("locale", "en-US")
	query.Set("deferred_intent[mode]", "subscription")
	query.Set("deferred_intent[setup_future_usage]", "off_session")
	query.Set("deferred_intent[payment_method_types][0]", "card")
	query.Set("deferred_intent[payment_method_types][1]", "link")
	if currency := strings.ToLower(s.CP.Currency); currency != "" {
		query.Set("currency", currency)
		query.Set("deferred_intent[currency]", currency)
	}
	if s.CP.Amount != nil {
		query.Set("deferred_intent[amount]", strconv.FormatInt(*s.CP.Amount, 10))
	}
	data, err := s.Call("GET", "/elements/sessions?"+query.Encode(), stripe_StripeCallOptions{})
	if err != nil {
		return err
	}
	id := protocol_AsString(protocol_FirstTruthy(data["elements_session_id"], protocol_AsMap(data["elements_session"])["id"], data["id"]))
	if strings.HasPrefix(id, "elements_session_") {
		s.ElementsID = id
	}
	if configID := protocol_AsString(protocol_FirstTruthy(data["config_id"], protocol_AsMap(data["elements_session"])["config_id"])); configID != "" {
		s.ConfigID = configID
	}
	return nil
}

func (s *stripe_Stripe) UpdateCustomCheckout(billing model_Billing, init protocol_Map) (protocol_Map, error) {
	if !strings.HasPrefix(s.CP.CheckoutID, "cs_live_") {
		return nil, protocol_NewError("checkout_id_invalid")
	}
	// Custom 税区更新与 Hosted 相同：只发 eid/tax_region。guid/muid/sid 会触发
	// parameter_unknown（Did you mean eid?）。响应 id/session_id 可能是 Elements
	// 会话，不拿来否定创建响应里已经确认的 Checkout ID。
	form := protocol_O("eid", protocol_FirstTruthy(init["eid"], "NA"), "tax_region", billing.Address(), "key", s.CP.PublishableKey, "_stripe_version", stripe_checkoutPageVersion(init, s.CP.StripeVersion))
	return s.Call("POST", "/payment_pages/"+s.CP.CheckoutID, stripe_StripeCallOptions{Form: form})
}

func (s *stripe_Stripe) HostedPrepare(raw string, billing model_Billing) (protocol_Map, error) {
	init, err := s.initializeHosted(raw)
	if err != nil {
		return nil, err
	}
	response, err := s.HTTP.Request("GET", strings.Split(s.HostedURL, "#")[0], protocol_RequestOptions{Headers: s.Profile.Headers()})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		return nil, &protocol_ProtocolError{Code: "hosted_page_unavailable"}
	}
	s.Started = time.Now()
	if err = s.loadHostedScripts(string(response.Content)); err != nil {
		return nil, err
	}
	s.hostedClientFingerprintWarmup()
	tax, err := s.Call("POST", "/payment_pages/"+s.CP.CheckoutID, stripe_StripeCallOptions{Hosted: true, Form: s.hostedTaxForm(protocol_AsString(init["eid"]), billing)})
	if err != nil {
		return nil, err
	}
	s.rememberHostedEid(tax)
	page, err := s.Page()
	if err != nil {
		return nil, err
	}
	s.hostedSnapshot = []protocol_Map{init, tax, page}
	checksum := protocol_AsString(init["init_checksum"])
	var scan func(any)
	scan = func(node any) {
		switch value := node.(type) {
		case protocol_Map:
			for _, key := range protocol_SortedKeys(value) {
				v := value[key]
				if text, ok := v.(string); strings.Contains(strings.ToLower(key), "checksum") && ok && text != "" {
					if len(text) > len(checksum) {
						checksum = text
					}
				} else {
					scan(v)
				}
			}
		case []any:
			for _, v := range value {
				scan(v)
			}
		}
	}
	scan(init)
	taxChecksum := protocol_AsString(tax["init_checksum"])
	if len(taxChecksum) > len(checksum) && len(taxChecksum) > 40 {
		checksum = taxChecksum
	}
	if checksum == "" {
		return nil, &protocol_ProtocolError{Code: "init_checksum_missing"}
	}
	var total any
	for _, data := range []protocol_Map{tax, init, page} {
		if value := stripe_HostedAmount(data); value != nil {
			total = *value
			break
		}
	}
	group := stripe_firstLineItemGroup(tax, init, page)
	return protocol_Map{"amount": total, "currency": protocol_FirstTruthy(tax["currency"], init["currency"], page["currency"], s.CP.Currency), "checksum": checksum, "eid": protocol_FirstTruthy(tax["eid"], init["eid"], "NA"), "config_id": protocol_AsString(protocol_FirstTruthy(tax["config_id"], init["config_id"], page["config_id"], "")), "line_item_group": group}, nil
}

func stripe_firstLineItemGroup(nodes ...protocol_Map) protocol_Map {
	for _, node := range nodes {
		if group := protocol_AsMap(node["line_item_group"]); len(group) > 0 {
			return group
		}
	}
	return nil
}

func (s *stripe_Stripe) ValidateHostedURL(raw string, completeOption ...bool) error {
	complete := true
	if len(completeOption) > 0 {
		complete = completeOption[0]
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || (parsed.Host != "checkout.stripe.com" && parsed.Host != "pay.openai.com") || parsed.Path != "/c/pay/"+s.CP.CheckoutID || (complete && (len(parsed.Fragment) < 250 || parsed.RawQuery != "")) {
		return &protocol_ProtocolError{Code: "hosted_url_invalid"}
	}
	return nil
}

func (s *stripe_Stripe) HostedForm(card model_Card, billing model_Billing, info protocol_Map) (protocol_Fields, error) {
	cp := s.CP
	attribution := protocol_O("client_session_id", s.ClientID, "checkout_session_id", cp.CheckoutID, "merchant_integration_source", "checkout", "merchant_integration_version", "hosted_checkout", "payment_method_selection_flow", "automatic", "elements_session_id", s.ElementsID)
	if protocol_Truthy(info["config_id"]) {
		attribution.Set("checkout_config_id", info["config_id"])
	}
	form := protocol_O("type", "card", "card", stripe_cardFields(card), "billing_details", protocol_O("name", billing.Name, "email", protocol_FirstTruthy(billing.Email, cp.Email), "address", billing.Address()), "payment_user_agent", "stripe.js/"+s.Build["version"]+"; stripe-js-v3/"+s.Build["version"]+"; checkout", "referrer", strings.Split(s.HostedURL, "#")[0], "time_on_page", s.TimeOnPage(), "key", cp.PublishableKey, "_stripe_version", stripe_HostedVersion)
	form.Update(s.IDs())
	form.Set("client_attribution_metadata", attribution)
	s.lookupCardMetadata(card)
	data, err := s.Call("POST", "/payment_methods", stripe_StripeCallOptions{Hosted: true, Form: form})
	if err != nil {
		return nil, err
	}
	pm := protocol_AsString(data["id"])
	if !strings.HasPrefix(pm, "pm_") {
		return nil, &protocol_ProtocolError{Code: "payment_method_missing"}
	}
	jsChecksum, err := stripe_ComputeJSChecksum(pm)
	if err != nil {
		return nil, err
	}
	rvTS, err := stripe_ComputeRVTimestamp(s.Build["rv_ts"], s.Build["rv"], s.Build["sv"])
	if err != nil {
		return nil, err
	}
	var expected any
	if cp.Amount != nil {
		expected = *cp.Amount
	}
	// 缺少 passive_captcha_token：真实浏览器在这里会带上 Stripe 通过 hCaptcha 被动模式
	// 取得的 JWT（真实抓包字段名就是 passive_captcha_token）。协议客户端无法执行
	// hCaptcha 的信号采集，因此拿不到该 token，服务端会据此升级为交互式人机验证，
	// 由 payment-action 的浏览器交接处理。详见 docs/payment-testing.md 的人机验证一节。
	result := protocol_O("eid", protocol_FirstTruthy(info["eid"], s.hostedEid, "NA"), "payment_method", pm, "expected_amount", expected, "expected_payment_method_type", "card", "link_brand", "link", "key", cp.PublishableKey, "_stripe_version", stripe_HostedVersion, "js_checksum", jsChecksum, "rv_timestamp", rvTS)
	// 确认与创建付款方式沿用同一组设备会话标识，不在阶段切换时重新生成。
	result.Update(s.IDs())
	result.Set("client_attribution_metadata", attribution)
	if s.HostedURL != "" {
		result.Set("referrer", s.HostedURL)
	}
	if details := stripe_lastDisplayedLineItemGroup(stripe_firstNonEmptyMap(info, s.hostedSnapshot...)); details != nil {
		result.Set("last_displayed_line_item_group_details", details)
	}
	result.Update(protocol_O("return_url", strings.Split(s.HostedURL, "#")[0], "consent", protocol_O("terms_of_service", "accepted")))
	return result, nil
}

func stripe_firstNonEmptyMap(info protocol_Map, extra ...protocol_Map) protocol_Map {
	out := protocol_Map{}
	for k, v := range info {
		out[k] = v
	}
	if len(protocol_AsMap(out["line_item_group"])) > 0 {
		return out
	}
	for i := len(extra) - 1; i >= 0; i-- {
		if group := protocol_AsMap(extra[i]["line_item_group"]); len(group) > 0 {
			out["line_item_group"] = group
			return out
		}
	}
	return out
}

func stripe_checkoutPageVersion(init protocol_Map, fallback string) string {
	base := strings.TrimSpace(fallback)
	if base == "" {
		base = stripe_CustomAPIVersion
	}
	betas := strings.TrimSpace(protocol_AsString(init["beta_versions"]))
	if betas == "" {
		return stripe_CustomCheckoutAPIVersion
	}
	if strings.Contains(base, betas) {
		return base
	}
	return base + "; " + betas
}

func stripe_consentRequired(init protocol_Map) bool {
	consent := protocol_AsMap(init["consent_collection"])
	return protocol_AsString(consent["terms_of_service"]) == "required"
}

func stripe_MergeCustomPaymentPage(dst, src protocol_Map) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range []string{"init_checksum", "eid", "config_id", "beta_versions", "automatic_payment_method_types", "consent_collection", "total_summary", "recurring_details", "invoice", "line_item_group"} {
		if src[key] != nil && src[key] != "" {
			dst[key] = src[key]
		}
	}
	if id := protocol_AsString(src["id"]); strings.HasPrefix(id, "ppage_") {
		dst["id"] = id
	}
}

func stripe_customClientBetas(init protocol_Map) []string {
	raw := protocol_AsString(init["beta_versions"])
	if raw == "" {
		raw = stripe_CustomCheckoutAPIVersion
	}
	var betas []string
	if strings.Contains(raw, "custom_checkout_beta=v1") {
		betas = append(betas, "custom_checkout_beta_6")
	}
	if strings.Contains(raw, "checkout_server_update_beta=v1") {
		betas = append(betas, "custom_checkout_server_updates_1")
	}
	if strings.Contains(raw, "checkout_manual_approval_preview=v1") {
		betas = append(betas, "custom_checkout_manual_approval_1")
	}
	return betas
}
