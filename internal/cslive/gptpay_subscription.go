// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	context "context"
	strings "strings"
	time "time"
)

// Subscription 查询 Session 所属个人账号的套餐和有效期，仅访问 GET 接口。
// 刷新凭据只保留在本次调用内，不创建 Checkout、不修改续费、不调用支付回调。
func (c *gptpay_PaymentClient) Subscription(ctx context.Context, session model_Session) (model_SubscriptionStatus, error) {
	var result model_SubscriptionStatus
	if err := c.validateConfig(); err != nil {
		return result, err
	}
	if strings.TrimSpace(session.AccessToken) == "" || strings.TrimSpace(session.SessionToken) == "" {
		return result, protocol_NewError("session_token_required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.Deadline*float64(time.Second)))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	deadline, _ := ctx.Deadline()
	factory := c.transportFactory
	if factory == nil {
		factory = func(ctx context.Context, proxy string, timeout float64, deadline time.Time) (protocol_HTTPTransport, error) {
			return transport_NewTransport(ctx, proxy, timeout, deadline)
		}
	}
	http, err := factory(ctx, c.Config.Proxy, c.Config.Timeout, deadline)
	if err != nil {
		return result, err
	}
	defer http.Close()
	profile, err := protocol_ResolveBrowserProfile(c.Config.BrowserProfile)
	if err != nil {
		return result, err
	}
	cp := model_DefaultCheckpoint()
	cp.AccessToken, cp.SessionToken, cp.Cookies = session.AccessToken, session.SessionToken, session.Cookies
	cp.AccountID, cp.Email, cp.Timezone = session.AccountID, session.Email, "UTC"
	clock, _ := fingerprint_NewClock(0)
	platform := chatgpt_NewPlatform(http, &cp, clock)
	platform.Profile = profile
	if err = platform.Homepage(); err != nil {
		return result, err
	}
	// 刷新接口可能返回 RefreshAccessTokenError，但已有访问令牌仍可读取账号。
	// 仅刷新失效时沿用原令牌，由后续账号接口验证；网络或账号不匹配错误仍终止。
	if err = platform.Refresh(); err != nil && protocol_FlowErrorCode(err) != "session_expired" {
		return result, err
	}
	account, err := platform.Account()
	if err != nil {
		return result, err
	}
	entitlement := protocol_AsMap(account["entitlement"])
	baseline := protocol_Map{
		"plan_type":               protocol_FirstTruthy(protocol_AsMap(account["account"])["plan_type"], entitlement["plan_type"]),
		"subscription_plan":       entitlement["subscription_plan"],
		"has_active_subscription": entitlement["has_active_subscription"],
	}
	subs, err := platform.Subscriptions(false)
	if protocol_FlowErrorCode(err) == "subscription_not_found" {
		subs, err = protocol_Map{}, nil
	}
	if err != nil {
		return result, err
	}
	result, err = gptpay_subscriptionStatus(baseline, subs)
	if err == nil {
		result.FetchedAt = time.Now().UTC()
	}
	return result, err
}

func gptpay_subscriptionStatus(baseline, subs protocol_Map) (model_SubscriptionStatus, error) {
	result := model_SubscriptionStatus{
		// 订阅记录可保留已失效套餐，当前套餐以账号接口为准。
		PlanType:         strings.TrimSpace(protocol_AsString(protocol_FirstTruthy(baseline["plan_type"], subs["plan_type"]))),
		SubscriptionPlan: strings.TrimSpace(protocol_AsString(protocol_FirstTruthy(subs["subscription_plan"], subs["plan"], baseline["subscription_plan"]))),
		Renewal:          chatgpt_RenewalState(subs),
	}
	if result.PlanType == "" {
		return model_SubscriptionStatus{}, protocol_NewError("subscription_status_invalid")
	}
	if active, ok := baseline["has_active_subscription"].(bool); ok {
		result.HasActiveSubscription = &active
	}
	if active, ok := subs["has_active_subscription"].(bool); ok {
		result.HasActiveSubscription = &active
	}
	// 只接受带时区的明确时间；未知值不能解释为永久有效或免费账号。
	if raw := protocol_FirstTruthy(subs["active_until"], subs["current_period_end"], subs["expires_at"], subs["billing_period_end"]); raw != nil {
		text, ok := raw.(string)
		if !ok {
			return model_SubscriptionStatus{}, protocol_NewError("subscription_status_invalid")
		}
		if strings.TrimSpace(text) != "" {
			until, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return model_SubscriptionStatus{}, protocol_NewError("subscription_status_invalid")
			}
			result.ActiveUntil = until.UTC().Format(time.RFC3339)
		}
	}
	return result, nil
}
