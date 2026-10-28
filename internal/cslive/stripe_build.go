// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	regexp "regexp"
)

// API 发布版本与 Stripe.js 的部署修订不同，保持当前 Basil 协议契约。
const stripe_CustomAPIVersion = "2025-03-31.basil"
const stripe_CustomJSURL = "https://js.stripe.com/basil/stripe.js"

// Custom Checkout Session 由 ChatGPT 带 beta 创建。payment_pages confirm
// 若不声明这些 beta，Stripe 会 HTTP 200 但会话保持 open/unpaid。
const stripe_CustomCheckoutAPIVersion = "2025-03-31.basil; custom_checkout_beta=v1; checkout_server_update_beta=v1; checkout_manual_approval_preview=v1"

var stripe_stripeJSBuildMarker = regexp.MustCompile(`/\*!\s*STRIPE_JS_BUILD_SALT\s+([a-f0-9]{10})\s*\*/`)
var stripe_stripeJSVersionLiteral = regexp.MustCompile(`^\s*(?:"([a-f0-9]{10})"|'([a-f0-9]{10})')`)

// ParseStripeJSVersion 只提取 payment_user_agent 使用的短构建标识。
// 注释中的 salt 与相邻版本字符串独立；分别检查一致性，不要求两者相等。
// 同名注释也修饰 40/64 位常量，不能把这些值或 Hosted 校验值混作版本。
func stripe_ParseStripeJSVersion(source string) (string, error) {
	markers := stripe_stripeJSBuildMarker.FindAllStringSubmatchIndex(source, -1)
	version := ""
	salt := ""
	for _, marker := range markers {
		value := source[marker[2]:marker[3]]
		if salt != "" && salt != value {
			return "", protocol_NewError("stripe_build_missing")
		}
		salt = value
		literal := stripe_stripeJSVersionLiteral.FindStringSubmatch(source[marker[1]:])
		if literal != nil {
			candidate := protocol_AsString(protocol_FirstTruthy(literal[1], literal[2]))
			if version != "" && candidate != version {
				return "", protocol_NewError("stripe_build_missing")
			}
			version = candidate
		}
	}
	if !stripe_validStripeJSVersion(version) {
		return "", protocol_NewError("stripe_build_missing")
	}
	return version, nil
}

func stripe_validStripeJSVersion(version string) bool {
	return version != "0000000000" && protocol_Matches(`[a-f0-9]{10}`, version)
}

func (s *stripe_Stripe) initializeBuild() error {
	response, err := s.HTTP.Request("GET", stripe_CustomJSURL, protocol_RequestOptions{Headers: s.Profile.Headers()})
	if err != nil {
		return err
	}
	if response == nil || response.StatusCode != 200 {
		return protocol_NewError("stripe_build_missing")
	}
	source := string(response.Content)
	version, err := stripe_ParseStripeJSVersion(source)
	if err != nil {
		return err
	}
	s.CP.StripeVersion = stripe_CustomAPIVersion
	s.JSVersion = version
	// Custom cs_live confirm 需要同一份 Basil 脚本里的 rv/sv；解析失败不阻断 oaics_。
	if build, buildErr := stripe_ParseStripeBuildConstants(source); buildErr == nil {
		s.Build = build
	}
	return nil
}
