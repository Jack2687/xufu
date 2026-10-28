// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	context "context"
	json "encoding/json"
	big "math/big"
	strings "strings"
	time "time"
)

type gptpay_CatalogPricesOptions struct {
	// ConfigKey 是页面实际使用的国家或币种键，例如 US、PHP；不自动猜测或回退。
	ConfigKey string
	// 可选断言用于识别上游地区回退；响应不匹配时整次查询失败。
	ExpectedCountry, ExpectedCurrency string
}

// CatalogPrices 仅请求价格配置 GET，不刷新 Session、不创建 Checkout、不初始化付款。
// 空 Session 使用公开查询；传入凭据时要求完整 Session，不自动刷新或降级。
func (c *gptpay_PaymentClient) CatalogPrices(ctx context.Context, session model_Session, options gptpay_CatalogPricesOptions) (result model_CatalogPrices, err error) {
	options.ConfigKey = strings.ToUpper(strings.TrimSpace(options.ConfigKey))
	options.ExpectedCountry = strings.ToUpper(strings.TrimSpace(options.ExpectedCountry))
	options.ExpectedCurrency = strings.ToUpper(strings.TrimSpace(options.ExpectedCurrency))
	if !protocol_Matches(`[A-Z]{2,3}`, options.ConfigKey) ||
		options.ExpectedCountry != "" && !protocol_Matches(`[A-Z]{2}`, options.ExpectedCountry) ||
		options.ExpectedCurrency != "" && !protocol_Matches(`[A-Z]{3}`, options.ExpectedCurrency) {
		return result, protocol_NewError("input_or_checkpoint_invalid")
	}
	if err = c.validateConfig(); err != nil {
		return result, err
	}
	if (strings.TrimSpace(session.AccessToken) == "") != (strings.TrimSpace(session.SessionToken) == "") {
		return result, protocol_NewError("session_token_required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Config.Deadline*float64(time.Second)))
	defer cancel()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	deadline, _ := ctx.Deadline()
	profile, err := protocol_ResolveBrowserProfile(c.Config.BrowserProfile)
	if err != nil {
		return result, err
	}
	obs := observability_NewObserver("catalog_prices", c.EventSink, "", "")
	obs.Emit("catalog_prices", protocol_Map{"outcome": "started"})
	defer func() {
		outcome := "completed"
		if err != nil {
			outcome = "failed"
		}
		obs.Emit("catalog_prices", protocol_Map{"outcome": outcome, "error_code": protocol_FlowErrorCode(err)})
	}()
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
	if h, ok := http.(interface {
		BindObserver(*observability_Observer, string)
	}); ok {
		h.BindObserver(obs, "chatgpt")
	}
	cp := model_DefaultCheckpoint()
	cp.AccessToken, cp.SessionToken, cp.Cookies = session.AccessToken, session.SessionToken, session.Cookies
	clock, _ := fingerprint_NewClock(0)
	platform := chatgpt_NewPlatform(http, &cp, clock)
	platform.Profile = profile
	data, err := platform.PricingConfig(options.ConfigKey)
	if err != nil {
		return result, err
	}
	result, err = gptpay_parseCatalogPrices(data, options)
	if err == nil {
		result.FetchedAt = time.Now().UTC()
	}
	return result, err
}

func gptpay_parseCatalogPrices(data protocol_Map, options gptpay_CatalogPricesOptions) (model_CatalogPrices, error) {
	fail := func() (model_CatalogPrices, error) {
		return model_CatalogPrices{}, protocol_NewError("catalog_prices_invalid")
	}
	country, ok := data["country_code"].(string)
	if !ok || !protocol_Matches(`[A-Z]{2}`, country) {
		return fail()
	}
	config, ok := data["currency_config"].(map[string]any)
	if !ok {
		return fail()
	}
	currency, ok := config["symbol_code"].(string)
	if !ok || !protocol_Matches(`[A-Z]{3}`, currency) {
		return fail()
	}
	exp, ok := protocol_Integer(config["minor_unit_exponent"])
	if !ok || exp < 0 || exp > 4 {
		return fail()
	}
	if options.ExpectedCountry != "" && country != options.ExpectedCountry || options.ExpectedCurrency != "" && currency != options.ExpectedCurrency {
		return model_CatalogPrices{}, protocol_NewError("catalog_prices_region_mismatch")
	}
	result := model_CatalogPrices{ConfigKey: options.ConfigKey, Country: country, Currency: currency, MinorUnitExponent: int(exp), DisplayOnly: true, Items: []model_CatalogPrice{}}
	for _, plan := range model_Capabilities().Plans {
		key := model_PricingConfigPlanKey(plan.ID)
		raw, exists := config[key]
		if !exists {
			continue
		}
		node, ok := raw.(map[string]any)
		if !ok {
			return fail()
		}
		// 只解析 SDK 支持的周期；未开放的年付折算价可能小于币种最小单位。
		for _, interval := range plan.Intervals {
			raw, exists := node[interval]
			if !exists {
				continue
			}
			price, ok := raw.(map[string]any)
			if !ok {
				return fail()
			}
			amount, ok := gptpay_catalogAmountMinor(price["amount"], int(exp))
			if !ok {
				return fail()
			}
			tax, ok := price["tax"].(string)
			if !ok || tax != "inclusive" && tax != "exclusive" {
				return fail()
			}
			basis := "month"
			if interval == "year" {
				basis = "unverified"
			}
			result.Items = append(result.Items, model_CatalogPrice{Plan: plan.ID, PriceKey: key, Interval: interval, AmountMinor: amount, Tax: tax, AmountBasis: basis, PerSeat: plan.Seats != nil})
		}
	}
	if len(result.Items) == 0 {
		return fail()
	}
	return result, nil
}

// 保留 JSON 十进制精度，拒绝隐式舍入、负数、超大指数与 int64 溢出。
func gptpay_catalogAmountMinor(value any, exponent int) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok || !protocol_Matches(`[0-9]{1,19}(\.[0-9]{1,8})?`, string(number)) {
		return 0, false
	}
	amount, ok := new(big.Rat).SetString(string(number))
	if !ok {
		return 0, false
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
	amount.Mul(amount, new(big.Rat).SetInt(scale))
	if !amount.IsInt() || !amount.Num().IsInt64() {
		return 0, false
	}
	return amount.Num().Int64(), true
}
