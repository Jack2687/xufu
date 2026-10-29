package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// These operations reproduce generator.py's long/short checkout requests.
// No card authorization, capture, subscription cancellation or payment success
// inference is performed here. Raw upstream responses never leave this module.
const checkoutEndpoint = "https://chatgpt.com/backend-api/payments/checkout"
const iosCheckoutUA = "ChatGPT/1.2026.196 (iOS 18.5; iPhone17,2; build 2682191628)"
const webCheckoutUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
const legacyCheckoutPublishableKey = "pk_live_51HOrSwC6h1nxGoI3lTAgRjYVrz4dU3fVOabyCcKR3pbEJguCVAlqCxdxCUvoRh1XWwRacViovU3kLKvpkjh7IqkW00iXQsjo3n"

var hostedCheckoutID = regexp.MustCompile(`^cs_(?:live|test)_[A-Za-z0-9]+$`)

type checkoutInput struct {
	CheckoutType                      string                     `json:"-"`
	BeforeCreate                      func(*checkoutInput) error `json:"-"`
	ObserveIP                         func(string) error         `json:"-"`
	AccessToken                       string                     `json:"access_token"`
	AccountID                         string                     `json:"account_id"`
	Email                             string                     `json:"email,omitempty"`
	SessionToken                      string                     `json:"session_token,omitempty"`
	Cookies                           string                     `json:"cookies,omitempty"`
	TokenExpiresAt                    time.Time                  `json:"token_expires_at,omitempty"`
	RefreshedAt                       time.Time                  `json:"refreshed_at,omitempty"`
	RefreshPurpose                    string                     `json:"refresh_purpose,omitempty"`
	Product, Country, Mode, Workspace string
	Seats, PremiumSeats               int
}
type checkoutOutput struct{ URL string }
type checkoutError struct {
	Code           string
	Retry, Unknown bool
}

func (e checkoutError) Error() string { return e.Code }
func checkoutFailure(code string, retry, unknown bool) error {
	return checkoutError{Code: code, Retry: retry, Unknown: unknown}
}

func providerJSON(ctx context.Context, c *http.Client, method, target string, headers http.Header, body []byte, write bool) (value M, failure error) {
	started := time.Now()
	var status int
	var requestID string
	ctx, timings := requestTimings(ctx)
	details := M{}
	defer func() {
		for k, v := range timings() {
			details[k] = v
		}
		reportProvider(ctx, status, requestID, failure, time.Since(started), details)
	}()
	req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if e != nil {
		return nil, checkoutFailure("invalid_request", false, false)
	}
	req.Header = headers.Clone()
	res, e := c.Do(req)
	if e != nil {
		// The bound proxy guard rejects locally before sending the business request.
		var guarded checkoutError
		if errors.As(e, &guarded) && (orderRenewalProxyTerminal(guarded) || guarded.Code == "proxy_unavailable") {
			return nil, guarded
		}
		details["network_reason"] = networkReason(e)
		return nil, checkoutFailure("network", !write, write)
	}
	defer res.Body.Close()
	status, requestID = res.StatusCode, res.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = res.Header.Get("Request-Id")
	}
	data, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if e != nil || len(data) > 1<<20 {
		return nil, checkoutFailure("invalid_response", !write, write)
	}
	details["response_kind"] = "other"
	if len(data) == 0 {
		details["response_kind"] = "empty"
	} else if strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		details["response_kind"] = "html"
	}
	var diagnostic M
	if json.Unmarshal(data, &diagnostic) == nil {
		details["response_kind"] = "json"
		node, _ := diagnostic["error"].(map[string]any)
		if node == nil {
			node, _ = diagnostic["detail"].(map[string]any)
		}
		details["provider_code"] = str(node, "code")
		details["decline_code"] = str(node, "decline_code")
	}
	if res.StatusCode == 401 {
		return nil, checkoutFailure("invalid_session", false, false)
	}
	if res.StatusCode == 403 {
		return nil, checkoutFailure("access_denied", false, false)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode == 404 {
			var missing struct {
				Detail struct {
					Code string `json:"code"`
				} `json:"detail"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &missing) == nil && (missing.Detail.Code == "subscription_not_found" || missing.Error.Code == "subscription_not_found") {
				return nil, checkoutFailure("subscription_not_found", true, false)
			}
		}
		if res.StatusCode >= 500 {
			return nil, checkoutFailure("upstream_unavailable", !write, write)
		}
		if res.StatusCode == 429 {
			return nil, checkoutFailure("upstream_busy", true, false)
		}
		return nil, checkoutFailure("request_rejected", false, false)
	}
	v := M{}
	if json.Unmarshal(data, &v) != nil {
		return nil, checkoutFailure("invalid_response", !write, write)
	}
	return v, nil
}

func checkoutPayload(in checkoutInput) (M, error) {
	currencies := map[string]string{"PH": "PHP", "US": "USD", "JP": "JPY", "SG": "SGD", "GB": "GBP", "EG": "EGP", "TR": "TRY"}
	currency, ok := currencies[in.Country]
	if !ok {
		return nil, errors.New("unsupported country")
	}
	if in.Mode != "long" && in.Mode != "short" {
		return nil, errors.New("unsupported checkout mode")
	}
	payload := M{"billing_details": M{"country": in.Country, "currency": currency}, "checkout_ui_mode": "hosted", "cancel_url": "https://chatgpt.com/#pricing"}
	if in.Mode == "short" {
		payload["checkout_ui_mode"] = "custom"
		payload["entry_point"] = "all_plans_pricing_modal"
		if in.Product == "team" || in.Product == "team_premium" {
			payload["entry_point"] = "team_workspace_purchase_modal"
		}
	}
	switch in.Product {
	case "pro":
		payload["plan_name"] = "chatgptpro"
	case "pro5x":
		payload["plan_name"] = "chatgptprolite"
	case "team", "team_premium":
		if in.Seats < 2 || in.Seats > 100 || in.PremiumSeats < 1 || in.PremiumSeats > 100 {
			return nil, errors.New("invalid seats")
		}
		payload["plan_name"] = "chatgptteamplan"
		team := M{"workspace_name": in.Workspace, "price_interval": "month"}
		if in.Product == "team" {
			team["seat_quantity"] = in.Seats
		} else {
			team["seat_quantities"] = []M{{"seat_type": "default", "quantity": in.Seats}, {"seat_type": "prolite", "quantity": in.PremiumSeats}}
		}
		payload["team_plan_data"] = team
	default:
		return nil, errors.New("unsupported plan")
	}
	return payload, nil
}

func validCheckoutURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || len(raw) > 16384 {
		return false
	}
	switch u.Hostname() {
	case "pay.openai.com", "checkout.stripe.com":
		return strings.HasPrefix(u.Path, "/c/pay/") || strings.HasPrefix(u.Path, "/pay/")
	case "chatgpt.com":
		return strings.HasPrefix(u.Path, "/checkout/")
	}
	return false
}

func validHostedCheckoutURL(raw string) bool {
	if !validCheckoutURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return u.Hostname() != "chatgpt.com" && hostedCheckoutID.MatchString(path.Base(u.Path)) && (u.Path == "/c/pay/"+path.Base(u.Path) || u.Path == "/pay/"+path.Base(u.Path))
}

var customCheckoutPath = regexp.MustCompile(`^/checkout/[a-z0-9_]{2,64}/(?:oaics_|cs_live_)[A-Za-z0-9]+$`)

func validCustomCheckoutURL(raw string) bool {
	if !validCheckoutURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return u.Host == "chatgpt.com" && u.RawQuery == "" && u.Fragment == "" && customCheckoutPath.MatchString(u.EscapedPath())
}

func generateCheckout(ctx context.Context, c *http.Client, in checkoutInput, beforeWrite func() error) (checkoutOutput, error) {
	var out checkoutOutput
	payload, e := checkoutPayload(in)
	if e != nil {
		return out, checkoutFailure("invalid_request", false, false)
	}
	// Unlike the original diagnostic script, never probe the direct connection,
	// and never silently replace the customer's selected billing region.
	ip, e := providerJSON(ctx, c, "GET", "https://ipinfo.io/json", http.Header{"Accept": {"application/json"}}, nil, false)
	if e != nil {
		return out, e
	}
	if strings.ToUpper(str(ip, "country")) != in.Country {
		return out, checkoutFailure("proxy_country_mismatch", true, false)
	}
	if in.ObserveIP != nil {
		parsed := net.ParseIP(str(ip, "ip"))
		if parsed == nil {
			return out, checkoutFailure("proxy_unavailable", true, false)
		}
		if e = in.ObserveIP(parsed.String()); e != nil {
			return out, e
		}
	}
	if in.BeforeCreate != nil {
		if e = in.BeforeCreate(&in); e != nil {
			return out, e
		}
	}
	headers := http.Header{"Authorization": {"Bearer " + in.AccessToken}, "Accept": {"application/json"}, "Content-Type": {"application/json"}}
	if in.Mode == "short" {
		headers.Set("User-Agent", webCheckoutUA)
		headers.Set("Origin", "https://chatgpt.com")
		headers.Set("Referer", "https://chatgpt.com/")
		headers.Set("OAI-Language", "en-US")
	} else {
		headers.Set("User-Agent", iosCheckoutUA)
		headers.Set("OAI-Language", "zh-Hans")
		headers.Set("OpenAI-Client-Type", "native")
		headers.Set("OpenAI-Client-Name", "chatgpt-ios")
		if in.AccountID != "" {
			headers.Set("ChatGPT-Account-ID", in.AccountID)
		}
	}
	if e = beforeWrite(); e != nil {
		return out, e
	}
	encoded, _ := json.Marshal(payload)
	result, e := providerJSON(ctx, c, "POST", checkoutEndpoint, headers, encoded, true)
	if e != nil {
		return out, e
	}
	cs := str(result, "checkout_session_id")
	if in.Mode == "short" {
		entity := str(result, "processor_entity")
		if entity == "" {
			entity = "openai_llc"
			if in.Country == "GB" { // The only European region currently offered by this app.
				entity = "openai_ie"
			}
		}
		out.URL = "https://chatgpt.com/checkout/" + entity + "/" + cs
		if len(cs) > 300 || !validCustomCheckoutURL(out.URL) {
			return checkoutOutput{}, checkoutFailure("invalid_response", false, true)
		}
		return out, nil
	}
	if len(cs) > 300 || !hostedCheckoutID.MatchString(cs) {
		return out, checkoutFailure("invalid_response", false, true)
	}
	if str(result, "tag") == "hosted_checkout_session" && str(result, "checkout_ui_mode") == "hosted" && validHostedCheckoutURL(str(result, "url")) {
		out.URL = str(result, "url")
	} else {
		pk := str(result, "publishable_key")
		if pk == "" {
			pk = legacyCheckoutPublishableKey
		}
		if !strings.HasPrefix(pk, "pk_") {
			return out, checkoutFailure("checkout_result_unknown", false, true)
		}
		form := url.Values{"key": {pk}, "eid": {"NA"}, "browser_locale": {"en-US"}, "browser_timezone": {"Asia/Manila"}, "redirect_type": {"url"}}
		hdr := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}, "Origin": {"https://pay.openai.com"}, "Referer": {"https://pay.openai.com/"}, "User-Agent": {webCheckoutUA}}
		initialized, err := providerJSON(ctx, c, "POST", "https://api.stripe.com/v1/payment_pages/"+url.PathEscape(cs)+"/init", hdr, []byte(form.Encode()), true)
		if err != nil {
			return out, checkoutFailure("checkout_result_unknown", false, true)
		}
		mode := str(initialized, "ui_mode")
		if mode == "" {
			mode = str(result, "checkout_ui_mode")
		}
		if mode != "hosted" || str(initialized, "success_url") == "" {
			return out, checkoutFailure("checkout_result_unknown", false, true)
		}
		out.URL = str(initialized, "stripe_hosted_url")
		if out.URL == "" {
			out.URL = str(initialized, "url")
		}
		// An absent URL remains unconfirmed; never fabricate a payable link.
	}
	out.URL = strings.Replace(out.URL, "https://checkout.stripe.com/", "https://pay.openai.com/", 1)
	if !validHostedCheckoutURL(out.URL) || (in.AccessToken != "" && strings.Contains(out.URL, in.AccessToken)) {
		return checkoutOutput{}, checkoutFailure("checkout_result_unknown", false, true)
	}
	u, _ := url.Parse(out.URL)
	if path.Base(u.Path) != cs {
		return checkoutOutput{}, checkoutFailure("checkout_result_unknown", false, true)
	}
	return out, nil
}
