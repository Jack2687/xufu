// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	base64 "encoding/base64"
	json "encoding/json"
	url "net/url"
	regexp "regexp"
	strings "strings"
)

var stripe_hostedPublishableKeyField = regexp.MustCompile(`"(?:apiKey|publishableKey|publishable_key)"\s*:\s*"(pk_(?:live|test)_[A-Za-z0-9]+)"`)

// HostedVersion 是 checkout.stripe.com 控制器的 apiVersion。
// 实抓 Hosted 页面为 Dahlia，不再使用 Basil + checkout_server_update_beta。
const stripe_HostedVersion = "2026-03-25.dahlia"

// ExtractHostedPublishableKeyFromURL 解码 Hosted fragment：URL unescape → Base64 → XOR 5 → JSON.apiKey。
// 这是 checkout 页面取 pk 的方式，不能抓 HTML 里 JS 包自带的其它 pk。
func stripe_ExtractHostedPublishableKeyFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	plain, err := stripe_decodeHostedCheckoutHash(parsed.Fragment)
	if err != nil {
		return ""
	}
	if key := protocol_AsString(protocol_FirstTruthy(plain["apiKey"], plain["publishableKey"], plain["publishable_key"])); protocol_Matches(`pk_[A-Za-z0-9_]+`, key) {
		return key
	}
	return ""
}

func stripe_decodeHostedCheckoutHash(fragment string) (protocol_Map, error) {
	raw := strings.TrimPrefix(fragment, "#")
	if raw == "" {
		return nil, protocol_NewError("hosted_url_invalid")
	}
	if unescaped, err := url.QueryUnescape(raw); err == nil {
		raw = unescaped
	}
	if pad := len(raw) % 4; pad != 0 {
		raw += strings.Repeat("=", 4-pad)
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, protocol_NewError("hosted_url_invalid")
	}
	for i := range data {
		data[i] ^= 5
	}
	var m protocol_Map
	if err := json.Unmarshal(stripe_bytesTrimSpace(data), &m); err != nil {
		if key := stripe_ExtractHostedPublishableKey(string(data)); key != "" {
			return protocol_Map{"apiKey": key}, nil
		}
		return nil, protocol_NewError("hosted_url_invalid")
	}
	return m, nil
}

func stripe_bytesTrimSpace(p []byte) []byte {
	return []byte(strings.TrimSpace(string(p)))
}

// ExtractHostedPublishableKey 只接受带字段名的 pk，避免误用 checkout JS 包里的其它密钥。
func stripe_ExtractHostedPublishableKey(html string) string {
	if m := stripe_hostedPublishableKeyField.FindStringSubmatch(html); len(m) > 1 {
		return m[1]
	}
	return ""
}

func stripe_HostedAmount(data protocol_Map) *int64 {
	for _, path := range [][]string{{"total_summary", "due"}, {"total_summary", "total"}, {"amount_due"}, {"invoice", "amount_due"}, {"total_summary", "amount_due"}, {"amount_total"}} {
		var value any = data
		for _, key := range path {
			value = protocol_AsMap(value)[key]
		}
		if number, ok := protocol_Integer(value); ok && number >= 0 {
			return &number
		}
	}
	return nil
}

func stripe_FindHostedURL(data any, checkoutID string) string {
	if value, ok := data.(string); ok {
		parsed, err := url.Parse(value)
		if err == nil && parsed.Scheme == "https" && parsed.User == nil && (parsed.Host == "pay.openai.com" || parsed.Host == "checkout.stripe.com") && parsed.Path == "/c/pay/"+checkoutID && len(parsed.Fragment) >= 250 && parsed.RawQuery == "" {
			return value
		}
	}
	switch value := data.(type) {
	case map[string]any:
		for _, key := range protocol_SortedKeys(value) {
			if found := stripe_FindHostedURL(value[key], checkoutID); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range value {
			if found := stripe_FindHostedURL(item, checkoutID); found != "" {
				return found
			}
		}
	case protocol_Fields:
		for _, item := range value {
			if found := stripe_FindHostedURL(item.Value, checkoutID); found != "" {
				return found
			}
		}
	}
	return ""
}
