// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
)

type model_Config struct {
	BrowserProfile string
	Proxy          string
	Timeout        float64
	Deadline       float64
	Currency       string
	Timezone       string
}

func model_DefaultConfig() model_Config { return model_Config{Timeout: 45, Deadline: 240} }

func (c model_Config) String() string {
	return fmt.Sprintf("Config{Timeout:%g Deadline:%g Currency:%s Timezone:%s BrowserProfile:%s Proxy:redacted}", c.Timeout, c.Deadline, c.Currency, c.Timezone, c.BrowserProfile)
}

func (c model_Config) GoString() string { return c.String() }
