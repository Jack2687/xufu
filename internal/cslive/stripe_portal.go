// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	json "encoding/json"
	dep_html "html"
	url "net/url"
	regexp "regexp"
	strings "strings"
)

const stripe_portalHost = "pay.openai.com"

var stripe_preloadedJSON = regexp.MustCompile(`(?s)<script[^>]*id="preloaded_json"[^>]*>(.*?)</script>`)

type stripe_portalSession struct {
	url, key, id, version, publishableKey, secret string
}

func (s *stripe_Stripe) LoadPortal(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != stripe_portalHost || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/p/session") {
		return protocol_NewError("customer_portal_unavailable")
	}
	h := s.Profile.Headers()
	h.Update(protocol_O("Accept", "text/html,application/xhtml+xml", "Upgrade-Insecure-Requests", "1"))
	response, err := s.HTTP.Request("GET", raw, protocol_RequestOptions{Headers: h})
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return protocol_NewError("customer_portal_unavailable")
	}
	boot, err := stripe_parsePortalBootstrap(string(response.Content))
	if err != nil {
		return err
	}
	s.portal = stripe_portalSession{
		url:            raw,
		key:            protocol_AsString(boot["session_api_key"]),
		id:             protocol_AsString(boot["portal_session_id"]),
		version:        protocol_AsString(boot["current_version"]),
		publishableKey: protocol_AsString(boot["publishable_key"]),
		secret:         parsed.Query().Get("secret"),
	}
	if s.portal.secret == "" {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "p" && parts[1] == "session" {
			s.portal.secret = parts[2]
		}
	}
	if !strings.HasPrefix(s.portal.key, "ek_") || !strings.HasPrefix(s.portal.id, "bps_") || !strings.HasPrefix(s.portal.publishableKey, "pk_") || s.portal.version == "" {
		return protocol_NewError("customer_portal_unavailable")
	}
	s.CP.PublishableKey = s.portal.publishableKey
	s.HostedURL = raw
	return nil
}

func (s *stripe_Stripe) CreatePortalSetupIntent() (string, string, error) {
	if s.portal.id == "" {
		return "", "", protocol_NewError("customer_portal_unavailable")
	}
	data, err := s.Call("POST", "/billing_portal/sessions/"+s.portal.id+"/exp/setup_intents", stripe_StripeCallOptions{Headers: s.portalHeaders(), Form: protocol_O()})
	if err != nil {
		return "", "", err
	}
	id := protocol_AsString(data["id"])
	secret := protocol_AsString(data["client_secret"])
	if !strings.HasPrefix(id, "seti_") || !strings.Contains(secret, "_secret_") {
		return "", "", protocol_NewError("setup_intent_missing")
	}
	return id, secret, nil
}

func (s *stripe_Stripe) BindPortalElements(secret string) error {
	if s.portal.id == "" || !strings.Contains(secret, "_secret_") || !strings.HasPrefix(s.CP.PublishableKey, "pk_") {
		return protocol_NewError("setup_intent_missing")
	}
	query := url.Values{}
	query.Set("client_secret", secret)
	query.Set("key", s.CP.PublishableKey)
	query.Set("_stripe_version", s.portal.version)
	query.Set("type", "setup_intent")
	query.Set("elements_init_source", "payment_element")
	query.Set("referrer_host", stripe_portalHost)
	query.Set("locale", "en-US")
	data, err := s.Call("GET", "/elements/sessions?"+query.Encode(), stripe_StripeCallOptions{})
	if err != nil {
		return err
	}
	id := stripe_findElementsSessionID(data)
	if !strings.HasPrefix(id, "elements_session_") {
		return protocol_NewError("stripe_build_missing")
	}
	s.ElementsID = id
	if configID := protocol_AsString(protocol_FirstTruthy(data["config_id"], protocol_AsMap(data["elements_session"])["config_id"])); configID != "" {
		s.ConfigID = configID
	}
	return nil
}

func (s *stripe_Stripe) PortalConfirmationToken(card model_Card, billing model_Billing) (string, error) {
	if !stripe_validStripeJSVersion(s.JSVersion) {
		return "", protocol_NewError("stripe_build_missing")
	}
	m := s.portalAttribution()
	details := protocol_O("name", billing.Name, "phone", "", "address", billing.Address())
	pm := protocol_O("type", "card", "card", stripe_cardFields(card), "allow_redisplay", "limited", "billing_details", details, "payment_user_agent", s.portalUserAgent()+"; deferred-intent", "referrer", s.portalReferrer(), "time_on_page", s.TimeOnPage())
	pm.Update(s.IDs())
	pm.Set("client_attribution_metadata", m)
	form := protocol_O("payment_method_data", pm, "client_attribution_metadata", m, "setup_future_usage", "off_session", "set_as_default_payment_method", "true", "key", s.CP.PublishableKey, "_stripe_version", s.portal.version, "client_context", protocol_O("mode", "setup", "currency", strings.ToLower(s.CP.Currency), "payment_method_types", []string{"card"}))
	data, err := s.Call("POST", "/confirmation_tokens", stripe_StripeCallOptions{Form: form})
	if err != nil {
		return "", err
	}
	id := protocol_AsString(data["id"])
	if !strings.HasPrefix(id, "ctoken_") {
		return "", protocol_NewError("confirmation_token_missing")
	}
	return id, nil
}

func (s *stripe_Stripe) PortalAPIVersion() string {
	return s.portal.version
}

func (s *stripe_Stripe) ConfirmPortalSetupIntent(id, secret, paymentMethodID, confirmationToken string) (string, error) {
	if !strings.HasPrefix(id, "seti_") || !strings.Contains(secret, "_secret_") {
		return "", protocol_NewError("setup_intent_missing")
	}
	if confirmationToken == "" && !strings.HasPrefix(paymentMethodID, "pm_") {
		return "", protocol_NewError("payment_method_missing")
	}
	form := protocol_O("client_secret", secret, "return_url", s.portalReturnURL(), "key", s.CP.PublishableKey, "_stripe_version", s.portal.version, "client_attribution_metadata", s.portalAttribution())
	if confirmationToken != "" {
		form.Set("confirmation_token", confirmationToken)
	} else {
		form.Set("payment_method", paymentMethodID)
		form.Set("mandate_data", protocol_O("customer_acceptance", protocol_O("type", "online", "online", protocol_O("infer_from_client", "true"))))
	}
	data, err := s.Call("POST", "/setup_intents/"+id+"/confirm", stripe_StripeCallOptions{Form: form, AllowError: true})
	if err != nil {
		return "", err
	}
	pm := stripe_setupIntentPaymentMethod(data)
	if stripe_setupIntentSucceeded(data, pm) {
		if pm == "" {
			pm = paymentMethodID
		}
		return pm, nil
	}
	if confirmationToken == "" && paymentMethodID != "" {
		portalForm := protocol_O("payment_method", paymentMethodID, "return_url", s.portalReturnURL())
		data, err = s.Call("POST", "/billing_portal/sessions/"+s.portal.id+"/exp/setup_intents/"+id+"/confirm", stripe_StripeCallOptions{Headers: s.portalHeaders(), Form: portalForm, AllowError: true})
		if err != nil {
			return "", err
		}
		pm = stripe_setupIntentPaymentMethod(data)
		if stripe_setupIntentSucceeded(data, pm) {
			if pm == "" {
				pm = paymentMethodID
			}
			return pm, nil
		}
	}
	if strings.ToLower(protocol_AsString(data["status"])) == "requires_action" {
		return pm, protocol_NewError("payment_action_required")
	}
	return "", protocol_NewError("setup_intent_failed")
}

func stripe_setupIntentPaymentMethod(data protocol_Map) string {
	return protocol_AsString(protocol_FirstTruthy(data["payment_method"], protocol_AsMap(data["payment_method"])["id"]))
}

func (s *stripe_Stripe) SetPortalDefaultPaymentMethod(paymentMethodID string) error {
	if s.portal.id == "" || !strings.HasPrefix(paymentMethodID, "pm_") {
		return protocol_NewError("payment_method_missing")
	}
	_, err := s.Call("POST", "/billing_portal/sessions/"+s.portal.id+"/exp/set_customer_default_payment_instrument", stripe_StripeCallOptions{Headers: s.portalHeaders(), Form: protocol_O("payment_method", paymentMethodID), AllowError: true})
	return err
}

func (s *stripe_Stripe) portalHeaders() protocol_Fields {
	h := s.Headers(true)
	h.Update(protocol_O("Accept", "application/json", "Content-Type", "application/x-www-form-urlencoded", "Authorization", "Bearer "+s.portal.key, "Stripe-Version", s.portal.version))
	return h
}

func (s *stripe_Stripe) portalReturnURL() string {
	u, err := url.Parse(s.portal.url)
	if err != nil {
		return "https://" + stripe_portalHost + "/p/session/add-payment-method/return"
	}
	q := url.Values{}
	if s.portal.secret != "" {
		q.Set("secret", s.portal.secret)
	}
	u.Path = "/p/session/add-payment-method/return"
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

func (s *stripe_Stripe) portalReferrer() string {
	return strings.Split(s.portal.url, "#")[0]
}

func (s *stripe_Stripe) portalUserAgent() string {
	return "stripe.js/" + s.JSVersion + "; stripe-js-v3/" + s.JSVersion + "; payment-element"
}

func (s *stripe_Stripe) portalAttribution() protocol_Fields {
	return protocol_O("client_session_id", s.ClientID, "merchant_integration_source", "elements", "merchant_integration_subtype", "payment-element", "merchant_integration_version", "2021", "payment_method_selection_flow", "automatic", "elements_session_id", s.ElementsID, "elements_session_config_id", s.ConfigID)
}

func stripe_findElementsSessionID(v any) string {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "elements_session_") {
			return x
		}
	case protocol_Map:
		for _, key := range []string{"elements_session_id", "session_id", "id"} {
			if id := stripe_findElementsSessionID(x[key]); id != "" {
				return id
			}
		}
		if id := stripe_findElementsSessionID(x["elements_session"]); id != "" {
			return id
		}
		for _, val := range x {
			if id := stripe_findElementsSessionID(val); id != "" {
				return id
			}
		}
	case []any:
		for _, item := range x {
			if id := stripe_findElementsSessionID(item); id != "" {
				return id
			}
		}
	}
	return ""
}

func stripe_parsePortalBootstrap(raw string) (protocol_Map, error) {
	match := stripe_preloadedJSON.FindStringSubmatch(raw)
	if match == nil {
		return nil, protocol_NewError("customer_portal_unavailable")
	}
	js := strings.TrimSpace(dep_html.UnescapeString(match[1]))
	var boot protocol_Map
	if err := json.Unmarshal([]byte(js), &boot); err != nil {
		return nil, protocol_NewError("customer_portal_unavailable")
	}
	return boot, nil
}

func stripe_setupIntentSucceeded(data protocol_Map, paymentMethodID string) bool {
	if protocol_AsMap(data["error"])["message"] != nil || protocol_AsMap(data["error"])["code"] != nil {
		return false
	}
	status := strings.ToLower(protocol_AsString(data["status"]))
	if status != "succeeded" && status != "processing" {
		return false
	}
	pm := protocol_AsString(protocol_FirstTruthy(data["payment_method"], protocol_AsMap(data["payment_method"])["id"]))
	return pm == "" || pm == paymentMethodID
}
