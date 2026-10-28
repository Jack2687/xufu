// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	strings "strings"
	time "time"
)

var model_preSubmitPhases = protocol_StringSet("prepared", "initializing", "account_checked", "stripe_ready", "checkout_created", "priced", "quoted", "upgrade_priced")

var model_customPhases = protocol_StringSet("openai_confirm_checkout", "openai_confirmed", "stripe_confirm")

var model_subscriptionUpdatePhases = protocol_StringSet("subscription_update")

var model_hostedPhases = protocol_StringSet("hosted_confirm", "hosted_approve")

var model_postPaymentPhases = protocol_StringSet("activation", "cancellation", "cancellation_requested", "completed")

var model_metadataKeys = protocol_StringSet("attestation", "build", "client_version", "checkout_ui_mode", "hosted_url", "scripts", "session_id", "subscription_baseline", "trace_id", "fingerprint_clock_offset_hours", "browser_profile", "payment_elapsed_ms", "checkout_client_secret", "team_default_seats", "team_prolite_seats", "team_price_interval", "payment_flow", "updated_plan", "payment_method_id", "setup_intent_id", "estimated_price", "charged_price", "stripe_client_id")

// ValidateForRecovery 在创建任何网络运行时之前校验完整的 v2 恢复上下文。
func (c model_Checkpoint) ValidateForRecovery() error {
	if c.Version != model_CheckpointVersion {
		return fmt.Errorf("unsupported checkpoint version")
	}
	if c.Amount != nil && *c.Amount < 0 {
		return fmt.Errorf("invalid checkpoint amount")
	}
	if c.Metadata == nil || c.StripeIdentity == nil || c.ConfirmBody == nil {
		return fmt.Errorf("invalid checkpoint mappings")
	}
	for _, validate := range []func() error{c.validateMetadata, c.validateValues, c.validateIdentifiers, c.validateFlowContext} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c model_Checkpoint) validateValues() error {
	phase, payment := c.Phase, c.Payment
	known := model_preSubmitPhases[phase] || model_customPhases[phase] || model_hostedPhases[phase] || model_subscriptionUpdatePhases[phase] || model_postPaymentPhases[phase] || phase == "payment_result"
	if !known {
		return fmt.Errorf("unknown checkpoint phase")
	}
	valid := false
	switch {
	case model_preSubmitPhases[phase]:
		valid = payment == "not_attempted"
	case model_customPhases[phase] || model_subscriptionUpdatePhases[phase] || phase == "hosted_confirm":
		valid = payment == "unknown"
	case phase == "hosted_approve":
		valid = protocol_StringSet("unknown", "pending", "authorized")[payment]
	case phase == "payment_result":
		valid = protocol_StringSet("unknown", "pending", "action_required", "authorized", "charged", "not_charged")[payment]
	case model_postPaymentPhases[phase]:
		valid = payment == "charged"
	}
	if !valid {
		return fmt.Errorf("checkpoint phase and payment state do not match")
	}
	if model_CheckoutPlan(c.Plan) == "" || !protocol_Matches(`[A-Z]{2}`, c.Country) || !protocol_Matches(`[A-Z]{3}`, c.Currency) {
		return fmt.Errorf("invalid checkpoint plan or region")
	}
	if _, e := time.LoadLocation(c.Timezone); e != nil || c.Timezone == "" || c.Timezone == "Local" {
		return fmt.Errorf("invalid checkpoint timezone")
	}
	if c.AccessToken == "" || c.SessionToken == "" {
		return fmt.Errorf("checkpoint session credentials are required")
	}
	if !protocol_Matches(`[a-z0-9_]{2,64}`, c.Entity) {
		return fmt.Errorf("invalid checkpoint processor entity")
	}
	if !protocol_StringSet("pending", "active")[c.Activation] || !protocol_StringSet("unknown", "on", "off")[c.Renewal] {
		return fmt.Errorf("invalid checkpoint subscription state")
	}
	if e := protocol_ValidateProxy(c.Proxy); e != nil {
		return e
	}
	return nil
}

func (c model_Checkpoint) validateIdentifiers() error {
	mode := protocol_AsString(c.Metadata["checkout_ui_mode"])
	if c.CheckoutID != "" && !protocol_Matches(`(?:oaics_|cs_live_)[A-Za-z0-9]+`, c.CheckoutID) {
		return fmt.Errorf("invalid checkpoint checkout ID")
	}
	if c.PaymentIntentID != "" && !protocol_Matches(`pi_[A-Za-z0-9]+`, c.PaymentIntentID) {
		return fmt.Errorf("invalid checkpoint PaymentIntent ID")
	}
	if c.ClientSecret != "" && (!protocol_Matches(`pi_[A-Za-z0-9]+_secret_[A-Za-z0-9]+`, c.ClientSecret) || strings.Split(c.ClientSecret, "_secret_")[0] != c.PaymentIntentID) {
		return fmt.Errorf("invalid checkpoint PaymentIntent secret")
	}
	if c.PaymentIntentID != "" && c.ClientSecret == "" && mode == "custom" {
		return fmt.Errorf("custom PaymentIntent secret is required")
	}
	if c.ConfirmationToken != "" && !protocol_Matches(`ctoken_[A-Za-z0-9]+`, c.ConfirmationToken) {
		return fmt.Errorf("invalid checkpoint confirmation token")
	}
	if c.PublishableKey != "" && !protocol_Matches(`pk_[A-Za-z0-9_]+`, c.PublishableKey) {
		return fmt.Errorf("invalid checkpoint publishable key")
	}
	return nil
}

// IsPreSubmitPhase 表示尚未进入付款提交边界。
func model_IsPreSubmitPhase(phase string) bool { return model_preSubmitPhases[phase] }
