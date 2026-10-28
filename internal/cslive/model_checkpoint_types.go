// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	bytes "bytes"
	json "encoding/json"
	fmt "fmt"
	reflect "reflect"
)

const model_CheckpointVersion = 2

type model_Checkpoint struct {
	Version           int               `json:"version"`
	Phase             string            `json:"phase"`
	Payment           string            `json:"payment"`
	Plan              string            `json:"plan"`
	Country           string            `json:"country"`
	Currency          string            `json:"currency"`
	Amount            *int64            `json:"amount"`
	AccountID         string            `json:"account_id"`
	Email             string            `json:"email"`
	CheckoutID        string            `json:"checkout_id"`
	Entity            string            `json:"entity"`
	PublishableKey    string            `json:"publishable_key"`
	AccessToken       string            `json:"access_token"`
	SessionToken      string            `json:"session_token"`
	Cookies           string            `json:"cookies"`
	DeviceID          string            `json:"device_id"`
	ClientSecret      string            `json:"client_secret"`
	ConfirmationToken string            `json:"confirmation_token"`
	ReturnURL         string            `json:"return_url"`
	PaymentIntentID   string            `json:"payment_intent_id"`
	StripeVersion     string            `json:"stripe_version"`
	Proxy             string            `json:"proxy"`
	Timezone          string            `json:"timezone"`
	Metadata          protocol_Map      `json:"metadata"`
	StripeIdentity    map[string]string `json:"stripe_identity"`
	ConfirmBody       protocol_Fields   `json:"confirm_body"`
	Activation        string            `json:"activation"`
	Renewal           string            `json:"renewal"`
	CancelRequested   bool              `json:"cancel_requested"`
}

func model_DefaultCheckpoint() model_Checkpoint {
	return model_Checkpoint{Version: model_CheckpointVersion, Phase: "prepared", Payment: "not_attempted", Entity: "openai_llc", Metadata: protocol_Map{}, StripeIdentity: map[string]string{}, ConfirmBody: protocol_Fields{}, Activation: "pending", Renewal: "unknown", CancelRequested: true}
}

func (c model_Checkpoint) String() string { return "Checkpoint{redacted}" }

func (c model_Checkpoint) GoString() string { return c.String() }

func (c *model_Checkpoint) UnmarshalJSON(b []byte) error {
	m, e := protocol_DecodeMap(b)
	if e != nil {
		return fmt.Errorf("invalid checkpoint JSON")
	}
	typ := reflect.TypeOf(model_Checkpoint{})
	allowed := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := f.Tag.Get("json")
		if _, exists := m[name]; !exists {
			return fmt.Errorf("missing checkpoint field: %s", name)
		}
		allowed[name] = f.Type
	}
	for k, v := range m {
		t, ok := allowed[k]
		if !ok {
			return fmt.Errorf("unknown checkpoint field")
		}
		if k == "amount" {
			if v != nil {
				n, ok := protocol_Integer(v)
				if !ok || n < 0 {
					return fmt.Errorf("invalid checkpoint amount")
				}
			}
			continue
		}
		if v == nil {
			return fmt.Errorf("invalid checkpoint field type: %s", k)
		}
		switch t.Kind() {
		case reflect.String:
			if _, ok := v.(string); !ok {
				return fmt.Errorf("invalid checkpoint field type: %s", k)
			}
		case reflect.Bool:
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("invalid checkpoint field type: %s", k)
			}
		case reflect.Int:
			if _, ok := protocol_Integer(v); !ok {
				return fmt.Errorf("unsupported checkpoint version")
			}
		case reflect.Map, reflect.Slice:
			object, ok := v.(protocol_Map)
			if !ok {
				return fmt.Errorf("invalid checkpoint field type: %s", k)
			}
			if k == "stripe_identity" {
				for _, value := range object {
					if _, ok := value.(string); !ok {
						return fmt.Errorf("invalid checkpoint identity value")
					}
				}
			}
		}
	}
	type plain model_Checkpoint
	var v plain
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e = d.Decode(&v); e != nil {
		return fmt.Errorf("invalid checkpoint field type")
	}
	*c = model_Checkpoint(v)
	return nil
}

func (c model_Checkpoint) Clone() (model_Checkpoint, error) {
	b, e := json.Marshal(c)
	if e != nil {
		return model_Checkpoint{}, fmt.Errorf("invalid checkpoint")
	}
	var out model_Checkpoint
	if e = json.Unmarshal(b, &out); e != nil {
		return model_Checkpoint{}, e
	}
	return out, nil
}

type model_CheckpointSink func(model_Checkpoint) error
