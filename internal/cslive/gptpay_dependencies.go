// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	context "context"
	time "time"
)

type gptpay_transportFactory func(context.Context, string, float64, time.Time) (protocol_HTTPTransport, error)

type gptpay_tokenSource interface {
	Token(flow string) ([2]string, error)
}
