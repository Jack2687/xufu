// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
)

type model_Billing struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Line1      string `json:"line1"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

func (b model_Billing) Address() protocol_Fields {
	return protocol_O("line1", b.Line1, "city", b.City, "state", b.State, "postal_code", b.PostalCode, "country", b.Country)
}

func (b model_Billing) Validate() error {
	if b.Name == "" || b.Line1 == "" || b.City == "" || b.PostalCode == "" || !protocol_Matches(`[A-Z]{2}`, b.Country) {
		return fmt.Errorf("billing name and address are required")
	}
	return nil
}
