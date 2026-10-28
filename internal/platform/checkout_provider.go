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
	"strings"
)

// These operations reproduce generator.py's hosted/custom checkout requests.
// No card authorization, capture, subscription cancellation or payment success
// inference is performed here. Raw upstream responses never leave this module.
const checkoutEndpoint = "https://chatgpt.com/backend-api/payments/checkout"
const iosCheckoutUA = "ChatGPT/1.2026.196 (iOS 18.5; iPhone17,2; build 2682191628)"
const webCheckoutUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

type checkoutInput struct {
	ObserveIP                         func(string) error `json:"-"`
	AccessToken                       string             `json:"access_token"`
	AccountID                         string             `json:"account_id"`
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
	return checkoutError{code, retry, unknown}
}

func providerJSON(ctx context.Context, c *http.Client, method, target string, headers http.Header, body []byte, write bool) (M, error) {
	req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if e != nil {
		return nil, checkoutFailure("invalid_request", false, false)
	}
	req.Header = headers.Clone()
	res, e := c.Do(req)
	if e != nil {
		return nil, checkoutFailure("network", !write, write)
	}
	defer res.Body.Close()
	data, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if e != nil || len(data) > 1<<20 {
		return nil, checkoutFailure("invalid_response", !write, write)
	}
	if res.StatusCode == 401 {
		return nil, checkoutFailure("invalid_session", false, false)
	}
	if res.StatusCode == 403 {
		return nil, checkoutFailure("access_denied", false, false)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
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
	ui, entry := "hosted", ""
	if in.Mode == "short" {
		ui = "custom"
		entry = "all_plans_pricing_modal"
		if strings.HasPrefix(in.Product, "team") {
			entry = "team_workspace_purchase_modal"
		}
	}
	payload := M{"billing_details": M{"country": in.Country, "currency": currency}, "checkout_ui_mode": ui, "cancel_url": "https://chatgpt.com/#pricing"}
	if entry != "" {
		payload["entry_point"] = entry
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
	headers := http.Header{"Authorization": {"Bearer " + in.AccessToken}, "Accept": {"application/json"}, "Content-Type": {"application/json"}}
	if in.Mode == "long" {
		headers.Set("User-Agent", iosCheckoutUA)
		headers.Set("OAI-Language", "zh-Hans")
		headers.Set("OpenAI-Client-Type", "native")
		headers.Set("OpenAI-Client-Name", "chatgpt-ios")
		headers.Set("ChatGPT-Account-ID", in.AccountID)
	} else {
		headers.Set("User-Agent", webCheckoutUA)
		headers.Set("Origin", "https://chatgpt.com")
		headers.Set("Referer", "https://chatgpt.com/")
		headers.Set("OAI-Language", "en-US")
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
	if cs == "" || len(cs) > 300 || (in.Mode == "long" && !strings.HasPrefix(cs, "cs_")) {
		return out, checkoutFailure("invalid_response", false, true)
	}
	if in.Mode == "short" {
		entity := str(result, "processor_entity")
		if entity == "" {
			entity = "openai_llc"
			if in.Country == "GB" {
				entity = "openai_ie"
			}
		}
		out.URL = "https://chatgpt.com/checkout/" + url.PathEscape(entity) + "/" + url.PathEscape(cs)
	} else {
		out.URL = str(result, "url")
		if out.URL == "" {
			pk := str(result, "publishable_key")
			if !strings.HasPrefix(cs, "cs_") || !strings.HasPrefix(pk, "pk_") {
				return out, checkoutFailure("invalid_response", false, true)
			}
			form := url.Values{"key": {pk}, "eid": {"NA"}, "browser_locale": {"en-US"}, "browser_timezone": {"Asia/Manila"}, "redirect_type": {"url"}}
			hdr := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}, "Origin": {"https://pay.openai.com"}, "Referer": {"https://pay.openai.com/"}, "User-Agent": {webCheckoutUA}}
			initialized, err := providerJSON(ctx, c, "POST", "https://api.stripe.com/v1/payment_pages/"+url.PathEscape(cs)+"/init", hdr, []byte(form.Encode()), true)
			if err != nil {
				return out, checkoutFailure("checkout_result_unknown", false, true)
			}
			out.URL = str(initialized, "stripe_hosted_url")
			if out.URL == "" {
				out.URL = str(initialized, "url")
			}
			// No guessed URL: incomplete responses remain unconfirmed.
		}
	}
	if !validCheckoutURL(out.URL) || strings.Contains(out.URL, in.AccessToken) {
		return checkoutOutput{}, checkoutFailure("checkout_result_unknown", false, true)
	}
	return out, nil
}
