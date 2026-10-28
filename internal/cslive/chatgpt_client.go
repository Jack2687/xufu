// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	rand "crypto/rand"
	json "encoding/json"
	big "math/big"
	http "net/http"
	url "net/url"
	regexp "regexp"
	strconv "strconv"
	strings "strings"
)

const chatgpt_PlatformBase = "https://chatgpt.com"

type chatgpt_Platform struct {
	// PromoCode 仅存在于本次提链调用，不写支付快照。
	PromoCode string
	Profile   protocol_BrowserProfile
	HTTP      protocol_HTTPTransport
	CP        *model_Checkpoint
	Clock     fingerprint_Clock
}

type chatgpt_PlatformHeaderOptions struct{ Referer, Mode string }

type chatgpt_PlatformCallOptions struct {
	Body    any
	Headers protocol_Fields
	Mode    string
}

func chatgpt_NewPlatform(transport protocol_HTTPTransport, cp *model_Checkpoint, clock fingerprint_Clock) *chatgpt_Platform {
	if cp.Metadata == nil {
		cp.Metadata = protocol_Map{}
	}
	if cp.Cookies == "" {
		cp.Cookies = "__Secure-platform.session-token=" + cp.SessionToken + "; __Secure-next-auth.session-token=" + cp.SessionToken + "; __Secure-platform.callback-url=https%3A%2F%2Fchatgpt.com%2F; __Secure-next-auth.callback-url=https%3A%2F%2Fchatgpt.com%2F"
	}
	if _, exists := cp.Metadata["session_id"]; !exists {
		cp.Metadata["session_id"] = protocol_RandomUUID()
	}
	return &chatgpt_Platform{HTTP: transport, CP: cp, Clock: clock, Profile: protocol_DefaultBrowserProfile()}
}

func chatgpt_Observation() string {
	return "v1.r.p." + protocol_RandomAlphabet(10, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")
}

func chatgpt_telemetryInt(low, high int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(high-low+1)))
	if err != nil {
		panic("random source unavailable")
	}
	return low + int(n.Int64())
}

func chatgpt_Telemetry() string {
	value := []any{1, float64(chatgpt_telemetryInt(3800, 16000)) / 10, chatgpt_telemetryInt(3, 12), chatgpt_telemetryInt(80, 420), chatgpt_telemetryInt(18, 90), chatgpt_telemetryInt(1, 6), 0, chatgpt_telemetryInt(420, 1500)}
	result, _ := protocol_CompactJSON(value)
	return result
}

func (p *chatgpt_Platform) Absorb(response *protocol_HTTPResponse) {
	if response == nil {
		return
	}
	request := &http.Request{Header: http.Header{"Cookie": []string{p.CP.Cookies}}}
	jar := protocol_Fields{}
	for _, cookie := range request.Cookies() {
		jar.Set(cookie.Name, cookie.Value)
	}
	parsed := (&http.Response{Header: response.Headers}).Cookies()
	for _, cookie := range parsed {
		jar.Set(cookie.Name, cookie.Value)
	}
	var parts []string
	for _, entry := range jar {
		parts = append(parts, entry.Key+"="+protocol_AsString(entry.Value))
	}
	p.CP.Cookies = strings.Join(parts, "; ")
	if value := jar.Get("oai-did"); value != nil {
		p.CP.DeviceID = protocol_AsString(value)
	}
}

// EnsureDeviceID 供现成 Hosted 长链使用：不访问 ChatGPT 首页，只从已有 Cookie 或新 UUID 补齐设备标识。
func (p *chatgpt_Platform) EnsureDeviceID() {
	if p.CP.DeviceID == "" {
		req := &http.Request{Header: http.Header{"Cookie": []string{p.CP.Cookies}}}
		for _, cookie := range req.Cookies() {
			if cookie.Name == "oai-did" && cookie.Value != "" {
				p.CP.DeviceID = cookie.Value
				break
			}
		}
	}
	if p.CP.DeviceID == "" {
		p.CP.DeviceID = protocol_RandomUUID()
	}
	if p.CP.DeviceID != "" && !strings.Contains(p.CP.Cookies, "oai-did=") {
		if p.CP.Cookies != "" {
			p.CP.Cookies += "; "
		}
		p.CP.Cookies += "oai-did=" + p.CP.DeviceID
	}
}

func (p *chatgpt_Platform) Headers(path string, options ...chatgpt_PlatformHeaderOptions) protocol_Fields {
	opts := chatgpt_PlatformHeaderOptions{Referer: chatgpt_PlatformBase + "/", Mode: "payment"}
	if len(options) > 0 {
		if options[0].Referer != "" {
			opts.Referer = options[0].Referer
		}
		if options[0].Mode != "" {
			opts.Mode = options[0].Mode
		}
	}
	cp := p.CP
	h := protocol_O("Authorization", "Bearer "+cp.AccessToken, "Cookie", cp.Cookies, "User-Agent", p.Profile.UserAgent, "Accept", "*/*", "Accept-Language", "en-US,en;q=0.9", "Content-Type", "application/json", "Origin", chatgpt_PlatformBase, "Referer", opts.Referer, "oai-device-id", cp.DeviceID, "oai-language", "en-US", "oai-session-id", cp.Metadata["session_id"])
	for _, pair := range [][2]string{{"client_version", "oai-client-version"}, {"build", "oai-client-build-number"}, {"attestation", "oai-web-deployment-attestation"}} {
		if protocol_Truthy(cp.Metadata[pair[0]]) {
			h.Set(pair[1], cp.Metadata[pair[0]])
		}
	}
	target := path
	if strings.HasSuffix(path, "/taxes") || strings.HasSuffix(path, "/snapshot") {
		target = strings.ReplaceAll(path, "/backend-api", "/api/v1")
	}
	if opts.Mode == "renewal" {
		target = strings.ReplaceAll(path, "/backend-api", "/api/v1")
		h.Update(protocol_O("Accept", "application/json", "chatgpt-account-id", cp.AccountID, "x-openai-is-client-observation", chatgpt_Observation()))
	} else {
		h.Update(protocol_O("x-oai-is-client-observation", chatgpt_Observation(), "x-oai-is-pending-updates", `{"v":3,"updates":[]}`))
		if path != "/backend-api/subscriptions" {
			h.Update(p.Profile.Headers())
		}
		if !strings.HasSuffix(path, "/confirm") && !strings.HasSuffix(path, "/approve") && path != "/backend-api/payments/checkout" {
			h.Update(protocol_O("sec-fetch-dest", "empty", "sec-fetch-mode", "cors", "sec-fetch-site", "same-origin", "priority", "u=1, i"))
			if path != "/backend-api/subscriptions" {
				h.Update(p.Profile.ExtendedHints())
			}
		}
		if strings.Contains(path, "/payments/checkout") && (!(strings.HasSuffix(path, "/taxes") || strings.HasSuffix(path, "/update")) || cp.Plan != "plus") {
			h.Update(protocol_O("chatgpt-account-id", cp.AccountID, "platform-account-id", cp.AccountID))
		}
	}
	if strings.Contains(path, "/payments/") || strings.Contains(path, "/subscriptions") {
		h.Update(protocol_O("x-openai-target-path", target, "x-openai-target-route", target))
	}
	// 刷新、订阅查询和续费路径也必须覆盖传输层默认平台提示。
	h.Update(p.Profile.Headers())
	return h
}

func (p *chatgpt_Platform) Call(method, path string, options ...chatgpt_PlatformCallOptions) (protocol_Map, error) {
	opts := chatgpt_PlatformCallOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	route, err := url.Parse(path)
	if err != nil {
		return nil, &protocol_ProtocolError{Code: "invalid_request_url"}
	}
	headers := opts.Headers
	if len(headers) == 0 {
		headers = p.Headers(route.Path, chatgpt_PlatformHeaderOptions{Mode: opts.Mode})
	}
	response, err := p.HTTP.Request(method, chatgpt_PlatformBase+path, protocol_RequestOptions{Headers: headers, Body: opts.Body})
	if err != nil {
		return nil, err
	}
	p.Absorb(response)
	if chatgpt_isSubscriptionNotFound(method, route.EscapedPath(), response) {
		return nil, protocol_NewError("subscription_not_found")
	}
	return protocol_JSONResponse(response, false)
}

func (p *chatgpt_Platform) Refresh() error {
	cp := p.CP
	if cp.SessionToken == "" {
		return &protocol_ProtocolError{Code: "session_token_required"}
	}
	h := p.Headers("/api/auth/session", chatgpt_PlatformHeaderOptions{Mode: "renewal"})
	h.Delete("Authorization")
	h.Update(protocol_O("x-openai-target-path", "/api/auth/session", "x-openai-target-route", "/api/auth/session"))
	data, err := p.Call("GET", "/api/auth/session?refresh=true&reason=token_expired", chatgpt_PlatformCallOptions{Headers: h})
	if err != nil {
		return err
	}
	token := protocol_AsString(data["accessToken"])
	if token == "" || protocol_Truthy(data["error"]) {
		return &protocol_ProtocolError{Code: "session_expired"}
	}
	account := protocol_AsString(protocol_AsMap(data["account"])["id"])
	if account != "" && cp.AccountID != "" && account != cp.AccountID {
		return &protocol_ProtocolError{Code: "account_mismatch"}
	}
	cp.AccessToken = token
	if cp.AccountID == "" {
		cp.AccountID = account
	}
	if cp.Email == "" {
		cp.Email = protocol_AsString(protocol_AsMap(data["user"])["email"])
	}
	return nil
}

func (p *chatgpt_Platform) Homepage() error {
	h := p.Headers("")
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	response, err := p.HTTP.Request("GET", chatgpt_PlatformBase+"/", protocol_RequestOptions{Headers: h})
	if err != nil {
		return err
	}
	p.Absorb(response)
	if response.StatusCode != 200 {
		return &protocol_ProtocolError{Code: "homepage_unavailable"}
	}
	html := string(response.Content)
	for _, pair := range [][2]string{{"client_version", `data-build="([^"]+)"`}, {"build", `data-seq="([^"]+)"`}, {"attestation", `"webDeploymentAttestation":"([^"]+)"`}, {"session_id", `"sessionId":"([^"]+)"`}} {
		if match := regexp.MustCompile(pair[1]).FindStringSubmatch(html); len(match) > 1 {
			p.CP.Metadata[pair[0]] = match[1]
		}
	}
	scripts := []any{}
	seen := map[string]bool{}
	base, _ := url.Parse(chatgpt_PlatformBase + "/")
	for _, match := range regexp.MustCompile(`(?:src|href)=["']([^"']+\.js(?:\?[^"']*)?)["']`).FindAllStringSubmatch(html, -1) {
		relative, err := url.Parse(match[1])
		if err != nil {
			continue
		}
		script := base.ResolveReference(relative).String()
		if !seen[script] {
			seen[script] = true
			scripts = append(scripts, script)
			if len(scripts) == 24 {
				break
			}
		}
	}
	p.CP.Metadata["scripts"] = scripts
	if p.CP.DeviceID == "" {
		return &protocol_ProtocolError{Code: "device_id_missing"}
	}
	return nil
}

func (p *chatgpt_Platform) Account() (protocol_Map, error) {
	offset, err := p.Clock.TimezoneOffsetMin(p.CP.Timezone)
	if err != nil {
		return nil, err
	}
	data, err := p.Call("GET", "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min="+strconv.Itoa(offset))
	if err != nil {
		return nil, err
	}
	for _, value := range protocol_AsMap(data["accounts"]) {
		entry := protocol_AsMap(value)
		account := protocol_AsMap(entry["account"])
		if protocol_AsString(protocol_FirstTruthy(account["id"], account["account_id"])) == p.CP.AccountID {
			kind := strings.ToLower(protocol_AsString(account["account_type"]))
			if kind == "team" || kind == "business" || kind == "enterprise" {
				return nil, &protocol_ProtocolError{Code: "personal_account_required"}
			}
			return protocol_Map{"account": account, "entitlement": protocol_AsMap(entry["entitlement"])}, nil
		}
	}
	return nil, &protocol_ProtocolError{Code: "account_not_found"}
}

func (p *chatgpt_Platform) Checkout(planName string, token [2]string, uiMode string) (protocol_Map, error) {
	if uiMode == "" {
		uiMode = "redirect"
	}
	path := "/backend-api/payments/checkout"
	h, err := p.ChallengeHeaders(path, token, chatgpt_PlatformBase+"/#pricing", true)
	if err != nil {
		return nil, err
	}
	body := protocol_O("plan_name", planName, "billing_details", protocol_O("country", p.CP.Country, "currency", p.CP.Currency), "checkout_ui_mode", uiMode, "cancel_url", chatgpt_PlatformBase+"/#pricing")
	if uiMode != "hosted" {
		body.Set("entry_point", "all_plans_pricing_modal")
	}
	if model_IsTeamPlan(p.CP.Plan) {
		cfg := p.CP.TeamSeatConfig()
		body.Set("team_plan_data", protocol_O("workspace_name", "Team", "quantity", cfg.Total(), "seat_quantity", cfg.Total(), "price_interval", cfg.Interval, "seat_quantities", cfg.Quantities()))
		if p.PromoCode != "" {
			body.Set("promo_code", p.PromoCode)
		}
	}
	return p.Call("POST", path, chatgpt_PlatformCallOptions{Headers: h, Body: body})
}

func (p *chatgpt_Platform) ChallengeHeaders(path string, token [2]string, referer string, split bool) (protocol_Fields, error) {
	h := p.Headers(path, chatgpt_PlatformHeaderOptions{Referer: referer})
	telemetry := token[1]
	if !split {
		telemetry = chatgpt_Telemetry()
	}
	h.Update(protocol_O("openai-sentinel-token", token[0], "oai-telemetry", telemetry))
	if split {
		var payload protocol_Map
		if err := json.Unmarshal([]byte(token[0]), &payload); err != nil {
			return nil, &protocol_ProtocolError{Code: "invalid_sentinel_token"}
		}
		for _, pair := range [][2]string{{"p", "OpenAI-Sentinel-Proof-Token"}, {"t", "OpenAI-Sentinel-Turnstile-Token"}} {
			if protocol_Truthy(payload[pair[0]]) {
				h.Set(pair[1], payload[pair[0]])
			}
		}
	}
	return h, nil
}

func (p *chatgpt_Platform) Taxes(billing model_Billing) (protocol_Map, error) {
	if p.CP.Entity != "openai_llc" {
		return nil, &protocol_ProtocolError{Code: "unsupported_processor_entity"}
	}
	return p.Call("POST", "/backend-api/payments/checkout/taxes", chatgpt_PlatformCallOptions{Body: protocol_O("checkout_session_id", p.CP.CheckoutID, "checkout_email", protocol_FirstTruthy(billing.Email, p.CP.Email), "billing_country", billing.Country, "billing_name", billing.Name, "currency", strings.ToLower(p.CP.Currency), "processor_entity", p.CP.Entity, "billing_address", billing.Address())})
}
