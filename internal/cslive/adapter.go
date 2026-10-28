package cslive

import (
	"context"
	"time"
	_ "time/tzdata"
)

type Checkpoint = model_Checkpoint
type Session = model_Session
type Card = model_Card
type Billing = model_Billing
type Config = model_Config
type PayOptions = gptpay_PayOptions
type Result = model_Result
type Client = gptpay_PaymentClient
type Progress = model_Progress

type Hooks struct {
	Checkpoint    func(Checkpoint) error
	Event         func(map[string]any)
	Progress      func(Progress)
	BeforeRequest func(context.Context, string, string) error
}

func NewClient(config Config, hooks Hooks) *Client {
	c := gptpay_NewPaymentClient(config, gptpay_ClientOptions{
		Checkpoint: hooks.Checkpoint, EventSink: hooks.Event, ProgressSink: hooks.Progress,
	})
	c.transportFactory = func(ctx context.Context, proxy string, timeout float64, expires time.Time) (protocol_HTTPTransport, error) {
		t, err := transport_NewTransport(ctx, proxy, timeout, expires)
		if err == nil {
			t.beforeRequest = hooks.BeforeRequest
		}
		return t, err
	}
	return c
}

func ErrorCode(err error) string    { return protocol_FlowErrorCode(err) }
func Failure(code string) error     { return protocol_NewError(code) }
func DefaultCheckpoint() Checkpoint { return model_DefaultCheckpoint() }
