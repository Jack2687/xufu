// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	bytes "bytes"
	json "encoding/json"
	io "io"
	mime "mime"
	http "net/http"
	url "net/url"
	strings "strings"
)

const chatgpt_subscriptionsPath = "/backend-api/subscriptions"
const chatgpt_subscriptionNotFoundDetail = "No subscription found for account"
const chatgpt_subscriptionErrorBodyLimit = 4096

func (p *chatgpt_Platform) Subscriptions(renewal bool) (protocol_Map, error) {
	mode := "payment"
	if renewal {
		mode = "renewal"
	}
	return p.Call("GET", chatgpt_subscriptionsPath+"?account_id="+url.QueryEscape(p.CP.AccountID), chatgpt_PlatformCallOptions{Mode: mode})
}

// 该响应仅说明订阅查询尚无结果，不代表扣款失败，也不授权再次付款。
// 精确限制请求、状态和业务正文，避免把路由错误或通用404解释为尚未到账。
func chatgpt_isSubscriptionNotFound(method, path string, response *protocol_HTTPResponse) bool {
	if method != http.MethodGet || path != chatgpt_subscriptionsPath || response == nil || response.StatusCode != http.StatusNotFound || len(response.Content) > chatgpt_subscriptionErrorBodyLimit {
		return false
	}
	if contentType := response.Headers.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" && !(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")) {
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Content))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	detail := ""
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		if key == "detail" && json.Unmarshal(value, &detail) != nil {
			return false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	return detail == chatgpt_subscriptionNotFoundDetail
}

func (p *chatgpt_Platform) PaymentMethods() (protocol_Map, error) {
	return p.Call("GET", "/backend-api/payments/payment_methods?account_id="+url.QueryEscape(p.CP.AccountID))
}

func (p *chatgpt_Platform) PricingConfig(country string) (protocol_Map, error) {
	path := "/backend-api/checkout_pricing_config/configs/" + url.PathEscape(country)
	headers := p.Headers(path)
	if p.CP.AccessToken == "" {
		// 公开目录不发送空 Bearer 或合成 Session Cookie。
		public := protocol_Fields{}
		for _, field := range headers {
			if !strings.EqualFold(field.Key, "Authorization") && !strings.EqualFold(field.Key, "Cookie") {
				public = append(public, field)
			}
		}
		headers = public
	}
	return p.Call("GET", path, chatgpt_PlatformCallOptions{Headers: headers})
}

func (p *chatgpt_Platform) RenewSubscription() (protocol_Map, error) {
	return p.Call("POST", chatgpt_subscriptionsPath+"/renew", chatgpt_PlatformCallOptions{Body: protocol_O("account_id", p.CP.AccountID)})
}

func (p *chatgpt_Platform) CustomerPortal() (string, error) {
	data, err := p.Call("GET", "/backend-api/payments/customer_portal?account_id="+url.QueryEscape(p.CP.AccountID))
	if err != nil {
		return "", err
	}
	raw := protocol_AsString(data["url"])
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "pay.openai.com" || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/p/session") {
		return "", protocol_NewError("customer_portal_unavailable")
	}
	return raw, nil
}

func (p *chatgpt_Platform) UpdateSubscription(planName, paymentMethodID, setupIntentID string) (protocol_Map, error) {
	body := protocol_O("account_id", p.CP.AccountID, "updated_plan", planName)
	if paymentMethodID != "" {
		body.Set("payment_method_id", paymentMethodID)
	}
	if setupIntentID != "" {
		body.Set("setup_intent_id", setupIntentID)
	}
	return p.Call("POST", chatgpt_subscriptionsPath+"/update", chatgpt_PlatformCallOptions{Body: body})
}

func (p *chatgpt_Platform) CancelPendingUpdate(paymentIntentID string) (protocol_Map, error) {
	return p.Call("POST", chatgpt_subscriptionsPath+"/update/cancel_pending", chatgpt_PlatformCallOptions{Body: protocol_O("account_id", p.CP.AccountID, "expected_payment_intent_id", paymentIntentID)})
}

func (p *chatgpt_Platform) AttachPaymentMethod(paymentMethodID string) (protocol_Map, error) {
	return p.Call("POST", "/backend-api/payments/payment_methods", chatgpt_PlatformCallOptions{Body: protocol_O("account_id", p.CP.AccountID, "payment_method_id", paymentMethodID, "set_as_default", true)})
}
