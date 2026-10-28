// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
)

type model_Card struct {
	Number   string `json:"number"`
	CVC      string `json:"cvc"`
	ExpMonth string `json:"exp_month"`
	ExpYear  string `json:"exp_year"`
}

func (c model_Card) String() string { return "Card{redacted}" }

func (c model_Card) GoString() string { return c.String() }

func (c model_Card) Validate() error {
	if !protocol_Matches(`[0-9]{12,19}`, c.Number) || !protocol_Matches(`[0-9]{3,4}`, c.CVC) {
		return fmt.Errorf("invalid card number or CVC")
	}
	var m int
	if _, e := fmt.Sscanf(c.ExpMonth, "%d", &m); e != nil || !protocol_Matches(`[0-9]+`, c.ExpMonth) || m < 1 || m > 12 {
		return fmt.Errorf("invalid expiry month")
	}
	if !protocol_Matches(`[0-9]{4}`, c.ExpYear) {
		return fmt.Errorf("expiry year must have four digits")
	}
	return nil
}
