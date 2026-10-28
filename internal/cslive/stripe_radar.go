// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	strings "strings"
)

func (s *stripe_Stripe) CreateRadarSession() error {
	if strings.HasPrefix(s.RadarSessionID, "rse_") {
		return nil
	}
	if !stripe_validStripeJSVersion(s.JSVersion) {
		return protocol_NewError("stripe_build_missing")
	}
	identity := s.CP.StripeIdentity
	if identity["guid"] == "" || identity["muid"] == "" || identity["sid"] == "" {
		return protocol_NewError("stripe_identity_missing")
	}
	form := protocol_O(
		"guid", identity["guid"],
		"muid", identity["muid"],
		"sid", identity["sid"],
		"referrer", "https://chatgpt.com",
		"payment_user_agent", "stripe.js/"+s.JSVersion+"; stripe-js-v3/"+s.JSVersion+"; payment-element; deferred-intent",
		"key", s.CP.PublishableKey,
		"_stripe_version", s.CP.StripeVersion,
	)
	data, err := s.Call("POST", "/radar/session", stripe_StripeCallOptions{Form: form, AllowError: true})
	if err != nil {
		return err
	}
	id := protocol_AsString(data["id"])
	if !strings.HasPrefix(id, "rse_") {
		if code := protocol_AsString(protocol_AsMap(data["error"])["code"]); code != "" {
			return protocol_NewError(code)
		}
		return protocol_NewError("radar_session_missing")
	}
	s.RadarSessionID = id
	return nil
}

func (s *stripe_Stripe) radarOptions() protocol_Fields {
	if !strings.HasPrefix(s.RadarSessionID, "rse_") {
		return nil
	}
	return protocol_O("session", s.RadarSessionID)
}
