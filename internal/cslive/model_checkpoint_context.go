// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	url "net/url"
	strings "strings"
)

func (c model_Checkpoint) validateFlowContext() error {
	if protocol_AsString(c.Metadata["payment_flow"]) == "subscription_update" {
		return c.validateSubscriptionUpdateContext()
	}
	phase := c.Phase
	mode := protocol_AsString(c.Metadata["checkout_ui_mode"])
	accountPhase := phase != "prepared" && phase != "initializing"
	stripePhase := accountPhase && phase != "account_checked"
	checkoutPhase := stripePhase && phase != "stripe_ready"
	pricedPhase := checkoutPhase && phase != "checkout_created"
	if accountPhase && (c.AccountID == "" || c.DeviceID == "") {
		return fmt.Errorf("checkpoint account context is incomplete")
	}
	if stripePhase && (c.PublishableKey == "" || c.StripeVersion == "" || c.StripeIdentity["guid"] == "" || c.StripeIdentity["muid"] == "" || c.StripeIdentity["sid"] == "") {
		return fmt.Errorf("checkpoint Stripe context is incomplete")
	}
	if checkoutPhase && (c.CheckoutID == "" || mode == "") {
		return fmt.Errorf("checkpoint checkout context is incomplete")
	}
	if pricedPhase && c.Amount == nil {
		return fmt.Errorf("checkpoint amount is required")
	}
	if model_preSubmitPhases[phase] && (c.ClientSecret != "" || c.PaymentIntentID != "" || c.ConfirmationToken != "" || len(c.ConfirmBody) > 0) {
		return fmt.Errorf("pre-submit checkpoint contains payment artifacts")
	}
	if model_customPhases[phase] && (mode != "custom" || !c.hasCustomPaymentArtifact()) {
		return fmt.Errorf("custom checkpoint context is incomplete")
	}
	if (phase == "openai_confirmed" || phase == "stripe_confirm") && (c.PaymentIntentID == "" || c.ClientSecret == "") {
		return fmt.Errorf("custom PaymentIntent context is incomplete")
	}
	if phase == "stripe_confirm" && len(c.ConfirmBody) == 0 {
		return fmt.Errorf("custom confirmation body is required")
	}
	if model_hostedPhases[phase] && (mode != "redirect" || len(c.ConfirmBody) == 0) {
		return fmt.Errorf("hosted checkpoint context is incomplete")
	}
	if mode == "redirect" && (model_hostedPhases[phase] || phase == "payment_result" || model_postPaymentPhases[phase]) {
		s, ok := c.Metadata["hosted_url"].(string)
		u, e := url.Parse(s)
		if !ok || e != nil || u.Scheme != "https" || u.User != nil || !protocol_StringSet("checkout.stripe.com", "pay.openai.com")[u.Host] || u.EscapedPath() != "/c/pay/"+c.CheckoutID || u.RawQuery != "" || len(u.Fragment) < 250 {
			return fmt.Errorf("invalid hosted checkpoint URL")
		}
	}
	if err := c.validateSubscriptionState(); err != nil {
		return err
	}
	if (phase == "payment_result" || model_postPaymentPhases[phase]) && mode == "custom" {
		if !c.hasCustomPaymentArtifact() {
			return fmt.Errorf("custom payment context is incomplete")
		}
		if strings.HasPrefix(c.CheckoutID, "oaics_") && c.Payment != "not_charged" && (c.PaymentIntentID == "" || c.ClientSecret == "") {
			return fmt.Errorf("custom payment context is incomplete")
		}
	}
	return nil
}

func (c model_Checkpoint) validateSubscriptionUpdateContext() error {
	phase := c.Phase
	if c.CheckoutID != "" || protocol_AsString(c.Metadata["checkout_ui_mode"]) != "" {
		return fmt.Errorf("subscription update must not use checkout")
	}
	accountPhase := phase != "prepared" && phase != "initializing"
	if accountPhase && (c.AccountID == "" || c.DeviceID == "") {
		return fmt.Errorf("checkpoint account context is incomplete")
	}
	priced := protocol_StringSet("priced", "quoted", "upgrade_priced", "subscription_update", "payment_result")[phase] || model_postPaymentPhases[phase]
	if priced && c.Amount == nil {
		return fmt.Errorf("checkpoint amount is required")
	}
	if model_preSubmitPhases[phase] && (c.ClientSecret != "" || c.PaymentIntentID != "" || c.ConfirmationToken != "" || len(c.ConfirmBody) > 0) {
		return fmt.Errorf("pre-submit checkpoint contains payment artifacts")
	}
	return c.validateSubscriptionState()
}

func (c model_Checkpoint) validateSubscriptionState() error {
	phase := c.Phase
	if !model_postPaymentPhases[phase] && (c.Activation != "pending" || c.Renewal != "unknown") {
		return fmt.Errorf("checkpoint phase and subscription state do not match")
	}
	if protocol_StringSet("cancellation", "cancellation_requested", "completed")[phase] && c.Activation != "active" {
		return fmt.Errorf("checkpoint phase requires active subscription")
	}
	if protocol_StringSet("cancellation", "cancellation_requested")[phase] && !c.CancelRequested {
		return fmt.Errorf("cancellation requires cancellation request")
	}
	if phase == "completed" && c.CancelRequested && c.Renewal != "off" {
		return fmt.Errorf("completed cancellation requires renewal off")
	}
	return nil
}

func (c model_Checkpoint) hasCustomPaymentArtifact() bool {
	if strings.HasPrefix(c.ConfirmationToken, "ctoken_") {
		return true
	}
	if c.ConfirmBody == nil {
		return false
	}
	return strings.HasPrefix(protocol_AsString(c.ConfirmBody.Get("payment_method")), "pm_")
}
