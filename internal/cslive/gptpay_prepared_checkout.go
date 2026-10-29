package cslive

import "strings"

// PreparedCheckout is a private handoff from another checkout generator. It
// carries only the original checkout identity, URL and initialization fields;
// payment state and card data are never accepted from the generating stage.
type PreparedCheckout struct {
	CheckoutID, URL, ProcessorEntity, PublishableKey, ClientSecret, ReturnURL string
}

func (PreparedCheckout) String() string     { return "PreparedCheckout{redacted}" }
func (p PreparedCheckout) GoString() string { return p.String() }

func (t *gptpay_transaction) importCheckout(prepared PreparedCheckout, kind string) (protocol_Map, error) {
	if kind != "hosted" && kind != "oaics" {
		return nil, protocol_NewError("checkout_ui_mode_invalid")
	}
	if !protocol_Matches(`(?:oaics_|cs_live_)[A-Za-z0-9]+`, prepared.CheckoutID) || kind == "hosted" && !strings.HasPrefix(prepared.CheckoutID, "cs_live_") {
		return nil, protocol_NewError("checkout_id_invalid")
	}
	if !protocol_Matches(`[a-z0-9_]{2,64}`, prepared.ProcessorEntity) {
		return nil, protocol_NewError("processor_entity_invalid")
	}
	if kind == "hosted" {
		if stripe_FindHostedURL(prepared.URL, prepared.CheckoutID) == "" {
			return nil, protocol_NewError("hosted_checkout_url_missing")
		}
	} else if prepared.URL != chatgpt_PlatformBase+"/checkout/"+prepared.ProcessorEntity+"/"+prepared.CheckoutID {
		return nil, protocol_NewError("checkout_id_invalid")
	}
	if prepared.PublishableKey != "" && !protocol_Matches(`pk_[A-Za-z0-9_]+`, prepared.PublishableKey) {
		return nil, protocol_NewError("publishable_key_missing")
	}
	if prepared.ClientSecret != "" && !strings.HasPrefix(prepared.ClientSecret, prepared.CheckoutID+"_secret_") {
		return nil, protocol_NewError("checkout_id_invalid")
	}
	cp := t.cp
	cp.CheckoutID, cp.Entity, cp.PublishableKey = prepared.CheckoutID, prepared.ProcessorEntity, prepared.PublishableKey
	cp.Metadata["checkout_ui_mode"] = "custom"
	checkout := protocol_Map{"checkout_session_id": cp.CheckoutID, "processor_entity": cp.Entity, "publishable_key": cp.PublishableKey}
	if kind == "hosted" {
		cp.Metadata["checkout_ui_mode"] = "redirect"
		cp.Metadata["hosted_url"] = prepared.URL
		checkout["url"] = prepared.URL
	}
	if prepared.ClientSecret != "" {
		cp.Metadata["checkout_client_secret"] = prepared.ClientSecret
		checkout["client_secret"] = prepared.ClientSecret
	}
	if prepared.ReturnURL != "" {
		validated, err := chatgpt_ValidateReturnURL(prepared.ReturnURL, cp)
		if err != nil {
			return nil, err
		}
		cp.ReturnURL = validated
	}
	t.platform.EnsureDeviceID()
	// Persist the original identity before any payment preparation. No homepage,
	// session refresh, account precheck or Checkout POST is repeated here.
	if err := t.save("checkout_created"); err != nil {
		return nil, err
	}
	t.progress.emit(model_ProgressStageCheckout, model_ProgressRunning, "")
	if err := t.initializeStripe(); err != nil {
		return nil, err
	}
	return checkout, t.save("checkout_created")
}
