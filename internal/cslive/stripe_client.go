// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	url "net/url"
	strconv "strconv"
	strings "strings"
	sync "sync"
	time "time"
)

const stripe_StripeBase = "https://api.stripe.com/v1"

var stripe_stripePlugins = []string{"PDF Viewer", "Chrome PDF Viewer", "Chromium PDF Viewer", "Microsoft Edge PDF Viewer", "WebKit built-in PDF"}

type stripe_Stripe struct {
	Profile                                                                         protocol_BrowserProfile
	HTTP                                                                            protocol_HTTPTransport
	CP                                                                              *model_Checkpoint
	Clock                                                                           fingerprint_Clock
	Started                                                                         time.Time
	ClientID, ElementsID, ConfigID, JSVersion, HostedURL, RadarSessionID, hostedEid string
	Build                                                                           map[string]string
	hostedSnapshot                                                                  []protocol_Map
	extractedHostedInit                                                             protocol_Map
	portal                                                                          stripe_portalSession
	confirmLock                                                                     sync.Mutex
	confirmSubmitted                                                                bool
	observer                                                                        *observability_Observer
}

type stripe_StripeCallOptions struct {
	Form       any
	Hosted     bool
	Headers    protocol_Fields
	AllowError bool
}

func stripe_NewStripe(transport protocol_HTTPTransport, cp *model_Checkpoint, clock fingerprint_Clock) *stripe_Stripe {
	if cp.StripeIdentity == nil {
		cp.StripeIdentity = map[string]string{}
	}
	return &stripe_Stripe{Profile: protocol_DefaultBrowserProfile(), HTTP: transport, CP: cp, Clock: clock, Started: time.Now(), ClientID: protocol_RandomUUID(), ElementsID: "elements_session_" + protocol_RandomAlphabet(11, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"), ConfigID: protocol_RandomUUID(), Build: map[string]string{}}
}

func (s *stripe_Stripe) BindObserver(observer *observability_Observer) { s.observer = observer }

func (s *stripe_Stripe) Headers(hostedOption ...bool) protocol_Fields {
	hosted := len(hostedOption) > 0 && hostedOption[0]
	origin := "https://js.stripe.com"
	referer := origin + "/"
	if hosted {
		parsed, _ := url.Parse(s.HostedURL)
		if parsed != nil {
			origin = "https://" + parsed.Host
		}
		referer = strings.Split(s.HostedURL, "#")[0]
	}
	h := s.Profile.Headers()
	h.Update(protocol_O("Accept", "application/json", "Content-Type", "application/x-www-form-urlencoded", "Origin", origin, "Referer", referer))
	identity := s.CP.StripeIdentity
	if !hosted || origin == "https://checkout.stripe.com" {
		parts := []string{}
		for _, pair := range [][2]string{{"__stripe_mid", protocol_AsString(protocol_FirstTruthy(identity["mid"], identity["muid"]))}, {"__stripe_sid", protocol_AsString(protocol_FirstTruthy(identity["sid_cookie"], identity["sid"]))}} {
			if pair[1] != "" {
				parts = append(parts, pair[0]+"="+pair[1])
			}
		}
		if len(parts) > 0 {
			h.Set("Cookie", strings.Join(parts, "; "))
		}
	}
	if hosted {
		site := "cross-site"
		if origin == "https://checkout.stripe.com" {
			site = "same-site"
		}
		h.Update(protocol_O("sec-fetch-dest", "empty", "sec-fetch-mode", "cors", "sec-fetch-site", site))
	}
	return h
}

func (s *stripe_Stripe) Call(method, path string, options ...stripe_StripeCallOptions) (protocol_Map, error) {
	opts := stripe_StripeCallOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	h := opts.Headers
	if len(h) == 0 {
		h = s.Headers(opts.Hosted)
	}
	response, err := s.HTTP.Request(method, stripe_StripeBase+path, protocol_RequestOptions{Headers: h, Form: opts.Form})
	if err != nil {
		return nil, err
	}
	data, err := protocol_JSONResponse(response, opts.AllowError)
	if err != nil {
		return nil, err
	}
	providerError := protocol_AsMap(protocol_FirstTruthy(data["error"], data["last_payment_error"]))
	if len(providerError) > 0 && s.observer != nil {
		// 诊断失败不得改变真实付款结果或重置防重保护。
		func() {
			defer func() { _ = recover() }()
			s.observer.Emit("payment_error", protocol_Map{"service": "stripe", "method": method, "path": stripe_StripeBase + path, "outcome": "failed", "provider_code": providerError["code"], "decline_code": providerError["decline_code"], "request_id": response.Headers.Get("Request-Id")})
		}()
	}
	return data, nil
}

func (s *stripe_Stripe) Initialize() error {
	if err := s.initializeBuild(); err != nil {
		return err
	}
	return s.InitializeIdentity()
}

// InitializeIdentity 只建立 guid/muid/sid。Hosted 现成长链不加载 Custom basil 脚本。
//
// 身份缺失时连续发两次设备注册，两次均携带本实现的 MTree 特征与上下文。
// 已有完整身份则复用；第一次失败不阻断身份建立，身份只从第二次响应读取。
func (s *stripe_Stripe) InitializeIdentity() error {
	identity := s.CP.StripeIdentity
	if identity["guid"] != "" && identity["muid"] != "" && identity["sid"] != "" {
		return nil
	}
	_, _ = s.postStripeJSInit()
	response, err := s.postStripeJSInit()
	if err != nil {
		return err
	}
	data, err := protocol_JSONResponse(response, false)
	if err != nil {
		return err
	}
	cookies := response.Cookies
	identity = map[string]string{}
	for _, key := range []string{"guid", "muid", "sid"} {
		identity[key] = protocol_AsString(protocol_FirstTruthy(data[key], ""))
	}
	identity["mid"] = protocol_AsString(protocol_FirstTruthy(cookies["__stripe_mid"], identity["muid"]))
	identity["sid_cookie"] = protocol_AsString(protocol_FirstTruthy(cookies["__stripe_sid"], identity["sid"]))
	identity["muid"] = protocol_AsString(protocol_FirstTruthy(identity["muid"], identity["mid"]))
	identity["sid"] = protocol_AsString(protocol_FirstTruthy(identity["sid"], identity["sid_cookie"]))
	identity["guid"] = protocol_AsString(protocol_FirstTruthy(identity["guid"], cookies["m"], identity["muid"]))
	if identity["guid"] == "" || identity["muid"] == "" || identity["sid"] == "" {
		return &protocol_ProtocolError{Code: "stripe_identity_missing"}
	}
	s.CP.StripeIdentity = identity
	return nil
}

// stripeJSInitID 是 Stripe.js 的 stripe_js_id，浏览器发送的是原始 UUIDv4 文本。
// 它与 client_attribution_metadata.client_session_id 同源，不能改成去连字符的 32 位 hex，
// 否则同一会话在 m/6 注册与业务请求里会呈现两个不同的设备标识。
func (s *stripe_Stripe) stripeJSInitID() string {
	return s.ClientID
}

// deviceRegistration 构造 m/6 的请求体。
// 结构与编码、以及请求头都对齐真实浏览器抓包（见 mtree.go 的说明）：
// 顶层 a 承载特征对象、顶层 b 承载上下文对象，整个载荷 JSON → encodeURIComponent → base64。
// tag 使用官方脚本里的字面量模板（真实抓包中四个请求都发该字符串）。
// id 仍沿用本笔页面会话标识；官方使用的 md5(特征值拼接) 语义尚未启用，见文档说明。
func (s *stripe_Stripe) deviceRegistration(timings *stripe_mtreeTimings) (string, error) {
	// 总耗时必须在特征采集完成后读取，避免遗漏采集过程。
	features := s.deviceFeature(timings)
	payload := protocol_O(
		"v2", "1",
		"id", s.stripeJSInitID(),
		"t", timings.total(),
		"tag", stripe_mtreeTagTemplate,
		"src", "js",
		"a", features,
		"b", s.deviceContext(),
		"h", stripe_randomNonce(20),
	)
	return stripe_mtreeEncode(payload)
}

// postStripeJSInit 发送一次 m/6 设备注册。
// 载荷里的 tag 是官方脚本未替换的构建模板字面量，不随调用点变化。
func (s *stripe_Stripe) postStripeJSInit() (*protocol_HTTPResponse, error) {
	body, err := s.deviceRegistration(stripe_newMtreeTimings(nil))
	if err != nil {
		return nil, err
	}
	return s.HTTP.Request("POST", "https://m.stripe.com/6", protocol_RequestOptions{Headers: s.mtreeHeaders(), Text: &body})
}

func (s *stripe_Stripe) IDs() protocol_Fields {
	return protocol_O("guid", s.CP.StripeIdentity["guid"], "muid", s.CP.StripeIdentity["muid"], "sid", s.CP.StripeIdentity["sid"])
}

// TimeOnPage 返回页面停留时间（毫秒），以 Hosted 页面加载完成的时刻为起点。
// 真实用户在支付页停留通常为 8~30 秒；不足 8 秒时按 8 秒上报，避免所有交易呈现同一个固定值。
func (s *stripe_Stripe) TimeOnPage() string {
	elapsed := time.Since(s.Started).Milliseconds()
	if elapsed < 8000 {
		elapsed = 8000
	}
	return strconv.FormatInt(elapsed, 10)
}

func stripe_cardFields(card model_Card) protocol_Fields {
	return protocol_O("number", card.Number, "cvc", card.CVC, "exp_month", card.ExpMonth, "exp_year", card.ExpYear)
}

func (s *stripe_Stripe) Token(card model_Card, billing model_Billing, checkout protocol_Map) (string, error) {
	if !stripe_validStripeJSVersion(s.JSVersion) {
		return "", protocol_NewError("stripe_build_missing")
	}
	m := protocol_O("client_session_id", s.ClientID, "merchant_integration_source", "elements", "merchant_integration_subtype", "payment-element", "merchant_integration_version", "2021", "payment_intent_creation_flow", "deferred", "payment_method_selection_flow", "merchant_specified", "elements_session_id", s.ElementsID, "elements_session_config_id", s.ConfigID, "merchant_integration_additional_elements", []string{"expressCheckout", "payment", "address"})
	details := protocol_O("name", billing.Name, "phone", "", "address", billing.Address())
	pm := protocol_O("type", "card", "card", stripe_cardFields(card), "allow_redisplay", "limited", "billing_details", details, "payment_user_agent", "stripe.js/"+s.JSVersion+"; stripe-js-v3/"+s.JSVersion+"; payment-element; deferred-intent", "referrer", "https://chatgpt.com", "time_on_page", s.TimeOnPage())
	pm.Update(s.IDs())
	if s.observer != nil {
		outcome := "missing"
		if len(s.radarOptions()) > 0 {
			outcome = "attached"
		}
		s.observer.Emit("radar_attachment", protocol_Map{"outcome": outcome})
	}
	pm.Set("client_attribution_metadata", m)
	if opts := s.radarOptions(); opts != nil {
		pm.Set("radar_options", opts)
	}
	form := protocol_O("payment_method_data", pm, "client_attribution_metadata", m, "setup_future_usage", "off_session", "set_as_default_payment_method", "false", "key", s.CP.PublishableKey, "_stripe_version", s.CP.StripeVersion)
	context := protocol_Fields{}
	if protocol_Truthy(checkout["customer_session_client_secret"]) {
		context = protocol_O("currency", strings.ToLower(s.CP.Currency), "mode", "subscription", "payment_method_types", []string{"card", "link"})
	}
	for _, key := range []string{"stripe_customer_id", "customer_id", "customer"} {
		if strings.HasPrefix(protocol_AsString(checkout[key]), "cus_") {
			context.Set("customer", checkout[key])
			break
		}
	}
	if len(context) > 0 {
		form.Set("client_context", context)
	}
	data, err := s.Call("POST", "/confirmation_tokens", stripe_StripeCallOptions{Form: form})
	if err != nil {
		return "", err
	}
	id := protocol_AsString(data["id"])
	if !strings.HasPrefix(id, "ctoken_") {
		return "", &protocol_ProtocolError{Code: "confirmation_token_missing"}
	}
	return id, nil
}

// ConfirmForm 的 client_session_id 与 m/6 的 stripe_js_id 同源。
// 两者是同一个页面会话标识：浏览器不会在设备注册与付款确认之间换掉它，
// 之前这里每次调用都新建随机 UUID，会让同一笔交易出现两个设备会话标识。
func (s *stripe_Stripe) ConfirmForm() protocol_Fields {
	cp := s.CP
	return protocol_O("confirmation_token", cp.ConfirmationToken, "client_secret", cp.ClientSecret, "key", cp.PublishableKey, "return_url", cp.ReturnURL, "_stripe_version", cp.StripeVersion, "client_attribution_metadata", protocol_O("client_session_id", s.stripeJSInitID(), "merchant_integration_source", "l1"))
}

func (s *stripe_Stripe) beginPaymentSubmit() error {
	// 请求结果未知仍算已提交；幂等键不授权重新尝试付款。
	s.confirmLock.Lock()
	defer s.confirmLock.Unlock()
	if s.confirmSubmitted {
		return &protocol_ProtocolError{Code: "payment_confirmation_already_submitted"}
	}
	s.confirmSubmitted = true
	return nil
}

func (s *stripe_Stripe) Confirm() (protocol_Map, error) {
	if err := s.beginPaymentSubmit(); err != nil {
		return nil, err
	}
	h := s.Headers()
	h.Set("Idempotency-Key", "pi-confirm-"+s.CP.PaymentIntentID)
	return s.Call("POST", "/payment_intents/"+s.CP.PaymentIntentID+"/confirm", stripe_StripeCallOptions{Form: s.CP.ConfirmBody, Headers: h, AllowError: true})
}

func (s *stripe_Stripe) ConfirmCheckoutSession() (protocol_Map, error) {
	if err := s.beginPaymentSubmit(); err != nil {
		return nil, err
	}
	h := s.Headers()
	h.Set("Idempotency-Key", "checkout-confirm-"+s.CP.CheckoutID)
	return s.Call("POST", "/payment_pages/"+s.CP.CheckoutID+"/confirm", stripe_StripeCallOptions{Form: s.CP.ConfirmBody, Headers: h, AllowError: true})
}

func (s *stripe_Stripe) Retrieve() (protocol_Map, error) {
	cp := s.CP
	if cp.PaymentIntentID != "" && cp.ClientSecret != "" {
		return s.Call("GET", "/payment_intents/"+cp.PaymentIntentID+"?client_secret="+url.QueryEscape(cp.ClientSecret)+"&key="+url.QueryEscape(cp.PublishableKey)+"&_stripe_version="+url.QueryEscape(cp.StripeVersion), stripe_StripeCallOptions{AllowError: true})
	}
	if strings.HasPrefix(cp.CheckoutID, "cs_live_") && protocol_AsString(cp.Metadata["checkout_ui_mode"]) == "custom" {
		return s.CustomPage()
	}
	return s.Page()
}

func (s *stripe_Stripe) Page() (protocol_Map, error) {
	return s.Call("GET", s.hostedPagePath(), stripe_StripeCallOptions{Hosted: true, AllowError: true})
}

func (s *stripe_Stripe) CustomPage() (protocol_Map, error) {
	return s.Call("GET", "/payment_pages/"+s.CP.CheckoutID+"?key="+url.QueryEscape(s.CP.PublishableKey)+"&_stripe_version="+url.QueryEscape(stripe_CustomCheckoutAPIVersion)+"&expand%5B%5D=payment_intent", stripe_StripeCallOptions{AllowError: true})
}
