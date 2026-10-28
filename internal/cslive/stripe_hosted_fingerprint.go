// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	url "net/url"
	regexp "regexp"
	strings "strings"
)

var stripe_hostedScriptURL = regexp.MustCompile(`https://js\.stripe\.com/v3/fingerprinted/js/(?:checkout-app(?:-init)?|stripe)-[A-Za-z0-9]+\.js[^"'\s]*`)
var stripe_hostedInitializedScriptURL = regexp.MustCompile(`https://js\.stripe\.com/v3/stripe\.js\?stripeCheckoutInitialized=true[^"'\s]*`)
var stripe_hostedOuterURL = regexp.MustCompile(`["']([^"']*m-outer-[A-Za-z0-9_-]+\.html[^"']*)["']`)

func (s *stripe_Stripe) hostedInitForm() protocol_Fields {
	form := protocol_O("browser_locale", "en-US", "browser_timezone", s.CP.Timezone, "key", s.CP.PublishableKey, "eid", "NA", "redirect_type", "url")
	if s.HostedURL != "" {
		// Stripe.js init 把当前 hosted 地址（含 fragment）放进 referrer；HTTP Referer 仍去 fragment。
		form.Set("referrer", s.HostedURL)
	}
	return form
}

func (s *stripe_Stripe) hostedTaxForm(eid string, billing model_Billing) protocol_Fields {
	// 实抓 tax 更新只有 eid/tax_region/key，不带 _stripe_version 和 mids。
	return protocol_O("eid", protocol_FirstTruthy(eid, "NA"), "tax_region", billing.Address(), "key", s.CP.PublishableKey)
}

func (s *stripe_Stripe) hostedPagePath() string {
	query := url.Values{}
	query.Set("eid", protocol_AsString(protocol_FirstTruthy(s.hostedEid, "NA")))
	query.Set("key", s.CP.PublishableKey)
	return "/payment_pages/" + s.CP.CheckoutID + "?" + query.Encode()
}

func (s *stripe_Stripe) rememberHostedEid(data protocol_Map) {
	if eid := protocol_AsString(data["eid"]); eid != "" {
		s.hostedEid = eid
	}
}

func (s *stripe_Stripe) loadHostedScripts(html string) error {
	var buildSource string
	scripts := append(stripe_hostedScriptURL.FindAllString(html, -1), stripe_hostedInitializedScriptURL.FindAllString(html, -1)...)
	for _, raw := range scripts {
		response, err := s.HTTP.Request("GET", raw, protocol_RequestOptions{Headers: s.Profile.Headers()})
		if err != nil {
			return err
		}
		if response.StatusCode != 200 {
			return &protocol_ProtocolError{Code: "stripe_build_missing"}
		}
		if strings.Contains(raw, "/stripe-") || strings.Contains(raw, "stripeCheckoutInitialized=true") {
			buildSource = string(response.Content)
		}
		if err := s.loadHostedOuter(response.Content); err != nil {
			return err
		}
	}
	if buildSource == "" {
		response, err := s.HTTP.Request("GET", "https://js.stripe.com/v3", protocol_RequestOptions{Headers: s.Profile.Headers()})
		if err != nil {
			return err
		}
		if response.StatusCode != 200 {
			return &protocol_ProtocolError{Code: "stripe_build_missing"}
		}
		buildSource = string(response.Content)
		if err := s.loadHostedOuter(response.Content); err != nil {
			return err
		}
	}
	build, err := stripe_ParseStripeBuildConstants(buildSource)
	if err != nil {
		return err
	}
	s.Build = build
	return nil
}

// hostedClientFingerprintWarmup 在 Hosted 页面脚本加载后建立设备身份，
// 并补发付款元素挂载遥测。身份建立失败不构成付款证据，也不中止准备。
//
// InitializeIdentity 在身份缺失时发送两次设备注册，已有完整身份时直接复用。
// 页面预热不额外补发 m/6 注册。
func (s *stripe_Stripe) hostedClientFingerprintWarmup() {
	_ = s.InitializeIdentity()
	payload, err := protocol_CompactJSON(protocol_O("type", "measurement", "event_name", "paymentElement.mount", "metrics", protocol_O("duration", s.TimeOnPage()), "client_session_id", s.ClientID, "publish_key", s.CP.PublishableKey))
	if err != nil {
		return
	}
	h := s.Profile.Headers()
	h.Update(protocol_O("Accept", "*/*", "Content-Type", "text/plain", "Origin", "https://js.stripe.com", "Referer", "https://js.stripe.com/"))
	_, _ = s.HTTP.Request("POST", "https://r.stripe.com/b", protocol_RequestOptions{Headers: h, Text: &payload})
}

func (s *stripe_Stripe) loadHostedOuter(source []byte) error {
	outer := stripe_hostedOuterURL.FindSubmatch(source)
	if len(outer) < 2 {
		return nil
	}
	base, _ := url.Parse("https://js.stripe.com/v3/")
	relative, err := url.Parse(string(outer[1]))
	if err != nil {
		return nil
	}
	outerURL := base.ResolveReference(relative)
	if outerURL.Host != "js.stripe.com" {
		return nil
	}
	_, err = s.HTTP.Request("GET", outerURL.String(), protocol_RequestOptions{Headers: s.Profile.Headers()})
	return err
}

func (s *stripe_Stripe) lookupCardMetadata(card model_Card) {
	if len(card.Number) < 6 || !protocol_Matches(`[0-9]{6}`, card.Number[:6]) {
		return
	}
	query := url.Values{}
	query.Set("bin_prefix", card.Number[:6])
	query.Set("key", s.CP.PublishableKey)
	_, _ = s.HTTP.Request("GET", "https://api.stripe.com/edge-internal/card-metadata?"+query.Encode(), protocol_RequestOptions{Headers: s.Headers(true)})
}

func stripe_lastDisplayedLineItemGroup(info protocol_Map) protocol_Fields {
	group := protocol_AsMap(info["line_item_group"])
	if len(group) == 0 {
		return nil
	}
	var inclusive, exclusive, discount int64
	for _, item := range stripe_asObjectList(group["tax_amounts"]) {
		amount, _ := protocol_Integer(item["amount"])
		if item["inclusive"] == true {
			inclusive += amount
		} else {
			exclusive += amount
		}
	}
	for _, item := range stripe_asObjectList(group["discount_amounts"]) {
		amount, _ := protocol_Integer(item["amount"])
		discount += amount
	}
	shipping, _ := protocol_Integer(protocol_AsMap(group["shipping_rate"])["amount"])
	subtotal, _ := protocol_Integer(protocol_FirstTruthy(group["subtotal"], int64(0)))
	return protocol_O("subtotal", subtotal, "total_exclusive_tax", exclusive, "total_inclusive_tax", inclusive, "total_discount_amount", discount, "shipping_rate_amount", shipping)
}

func stripe_asObjectList(raw any) []protocol_Map {
	switch value := raw.(type) {
	case []any:
		out := make([]protocol_Map, 0, len(value))
		for _, item := range value {
			if m := protocol_AsMap(item); len(m) > 0 {
				out = append(out, m)
			}
		}
		return out
	case []protocol_Map:
		return value
	default:
		return nil
	}
}
