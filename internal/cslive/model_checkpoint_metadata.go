// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	strings "strings"
)

func (c model_Checkpoint) validateMetadata() error {
	if _, err := c.BrowserProfile(); err != nil {
		return err
	}
	for k := range c.Metadata {
		if !model_metadataKeys[k] {
			return fmt.Errorf("unknown checkpoint metadata field")
		}
	}
	if v, exists := c.Metadata["payment_elapsed_ms"]; exists {
		elapsed, valid := protocol_Integer(v)
		if !valid || elapsed < 0 || c.Payment != "charged" {
			return fmt.Errorf("invalid checkpoint payment elapsed time")
		}
	}
	if v, ok := c.Metadata["fingerprint_clock_offset_hours"]; ok {
		n, valid := protocol_Integer(v)
		if !valid || n != 0 && n != -24 {
			return fmt.Errorf("invalid fingerprint clock offset")
		}
	}
	if err := c.validateTeamSeatMetadata(); err != nil {
		return err
	}
	mode := protocol_AsString(c.Metadata["checkout_ui_mode"])
	if v, ok := c.Metadata["checkout_ui_mode"]; ok {
		if _, ok := v.(string); !ok || mode != "custom" && mode != "redirect" {
			return fmt.Errorf("invalid checkpoint checkout UI mode")
		}
	}
	for _, k := range []string{"attestation", "build", "client_version", "hosted_url", "session_id", "checkout_client_secret"} {
		if v, ok := c.Metadata[k]; ok {
			s, ok := v.(string)
			if !ok || k == "checkout_client_secret" && (c.CheckoutID == "" || !strings.HasPrefix(s, c.CheckoutID+"_secret_") || len(s) == len(c.CheckoutID+"_secret_")) {
				return fmt.Errorf("invalid checkpoint metadata field")
			}
		}
	}
	if v, ok := c.Metadata["stripe_client_id"]; ok {
		// 设备会话标识必须与 m/6 的 stripe_js_id 同源，恢复时不能接受被改写或类型错误的值。
		s, valid := v.(string)
		if !valid || !protocol_Matches(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`, s) {
			return fmt.Errorf("invalid checkpoint stripe client id")
		}
	}
	if v, ok := c.Metadata["trace_id"]; ok && v != nil {
		s, valid := v.(string)
		if !valid || !protocol_Matches(`[0-9a-f]{32}`, s) {
			return fmt.Errorf("invalid checkpoint trace ID")
		}
	}
	if v, ok := c.Metadata["scripts"]; ok && v != nil {
		switch x := v.(type) {
		case []string:
		case []any:
			for _, s := range x {
				if _, ok := s.(string); !ok {
					return fmt.Errorf("invalid checkpoint scripts")
				}
			}
		default:
			return fmt.Errorf("invalid checkpoint scripts")
		}
	}
	if v, ok := c.Metadata["subscription_baseline"]; ok && v != nil {
		m, ok := v.(protocol_Map)
		if !ok || len(m) != 4 {
			return fmt.Errorf("invalid subscription baseline")
		}
		for _, k := range []string{"plan_type", "subscription_plan", "cancels_at"} {
			if _, ok := m[k].(string); !ok {
				return fmt.Errorf("invalid subscription baseline")
			}
		}
		active, exists := m["has_active_subscription"]
		if !exists {
			return fmt.Errorf("invalid subscription baseline")
		}
		if active != nil {
			if _, ok := active.(bool); !ok {
				return fmt.Errorf("invalid subscription baseline")
			}
		}
	}
	for _, key := range []string{"estimated_price", "charged_price"} {
		if v, ok := c.Metadata[key]; ok && v != nil {
			charged := key == "charged_price"
			if charged && c.Payment != string(model_PaymentCharged) {
				return fmt.Errorf("charged price requires confirmed payment")
			}
			if e := model_validatePriceMetadata(v, charged); e != nil {
				return e
			}
		}
	}
	flow := protocol_AsString(c.Metadata["payment_flow"])
	if v, ok := c.Metadata["payment_flow"]; ok {
		if _, ok := v.(string); !ok || flow != "subscription_update" {
			return fmt.Errorf("invalid payment flow")
		}
	}
	plan := protocol_AsString(c.Metadata["updated_plan"])
	if v, ok := c.Metadata["updated_plan"]; ok {
		if _, ok := v.(string); !ok || plan != "chatgptprolite" {
			return fmt.Errorf("invalid updated plan")
		}
	}
	if (flow == "") != (plan == "") {
		return fmt.Errorf("subscription update metadata is incomplete")
	}
	if flow == "subscription_update" && c.Plan != "pro5x" {
		return fmt.Errorf("subscription update requires pro5x")
	}
	if v, ok := c.Metadata["payment_method_id"]; ok {
		s, ok := v.(string)
		if !ok || !protocol_Matches(`pm_[A-Za-z0-9]+`, s) {
			return fmt.Errorf("invalid payment method id")
		}
	}
	if v, ok := c.Metadata["setup_intent_id"]; ok {
		s, ok := v.(string)
		if !ok || !protocol_Matches(`seti_[A-Za-z0-9]+`, s) {
			return fmt.Errorf("invalid setup intent id")
		}
	}
	return nil
}

func (c model_Checkpoint) validateTeamSeatMetadata() error {
	if !model_IsTeamPlan(c.Plan) {
		for _, key := range []string{"team_default_seats", "team_prolite_seats", "team_price_interval"} {
			if _, exists := c.Metadata[key]; exists {
				return fmt.Errorf("team metadata only applies to team plans")
			}
		}
		return nil
	}
	defaultSeats, defaultOK := protocol_Integer(c.Metadata["team_default_seats"])
	proLiteSeats, proLiteOK := protocol_Integer(c.Metadata["team_prolite_seats"])
	maxSeats := int64(^uint(0) >> 1)
	if !defaultOK || !proLiteOK || defaultSeats < 0 || proLiteSeats < 0 || defaultSeats > maxSeats || proLiteSeats > maxSeats-defaultSeats || defaultSeats+proLiteSeats < model_MinTeamSeats {
		return fmt.Errorf("invalid checkpoint team seats")
	}
	if c.Plan == "team5x" && proLiteSeats == 0 {
		return fmt.Errorf("team5x checkpoint requires 5x seats")
	}
	interval, ok := c.Metadata["team_price_interval"].(string)
	if !ok || interval != "month" && interval != "year" {
		return fmt.Errorf("invalid checkpoint team price interval")
	}
	return nil
}

// BrowserProfile 只接受快照明确保存的版本 ID，恢复时不猜测或重新选择画像。
func (c model_Checkpoint) BrowserProfile() (protocol_BrowserProfile, error) {
	v, exists := c.Metadata["browser_profile"]
	if !exists {
		return protocol_BrowserProfile{}, fmt.Errorf("checkpoint browser profile is required")
	}
	id, ok := v.(string)
	if !ok {
		return protocol_BrowserProfile{}, fmt.Errorf("invalid checkpoint browser profile")
	}
	return protocol_LookupBrowserProfile(id)
}
