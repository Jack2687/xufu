package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type cardInput struct {
	Number string `json:"number"`
	Month  string `json:"month"`
	Year   string `json:"year"`
	CVC    string `json:"cvc"`
}
type paymentInput struct {
	Proxy          *browserProxyInput `json:"proxy"`
	ExpectedIP     string             `json:"expected_ip"`
	ProxyExpiresAt int64              `json:"proxy_expires_at"`
	Mode           string             `json:"mode"`
	URL            string             `json:"url"`
	Email          string             `json:"email"`
	Card           cardInput          `json:"card"`
	Billing        M                  `json:"billing"`
	PrepareSeconds int                `json:"prepare_seconds"`
	QuoteSeconds   int                `json:"quote_seconds"`
	ResultSeconds  int                `json:"result_seconds"`
	AutoConfirm    bool               `json:"-"`
	MaxAmount      int64              `json:"-"`
	Currency       string             `json:"-"`
}
type paymentEvent struct {
	Phase       string           `json:"phase,omitempty"`
	HTTPStatus  int              `json:"http_status,omitempty"`
	RequestID   string           `json:"request_id,omitempty"`
	ElapsedMS   int64            `json:"elapsed_ms,omitempty"`
	Diagnostics M                `json:"diagnostics,omitempty"`
	Evidence    *paymentEvidence `json:"evidence,omitempty"`
	Type        string           `json:"type"`
	State       string           `json:"state"`
	Code        string           `json:"code"`
	Sent        bool             `json:"sent"`
	Amount      int64            `json:"amount"`
	Currency    string           `json:"currency"`
}
type paymentEvidence struct {
	CheckoutID     string `json:"checkout_id"`
	IntentID       string `json:"payment_intent_id"`
	Status         string `json:"status"`
	AmountReceived *int64 `json:"amount_received"`
	Currency       string `json:"currency"`
}
type paymentCommand struct {
	Type string `json:"type"`
}
type paymentProcess interface {
	Events() <-chan paymentEvent
	Send(paymentCommand) error
	Close()
}
type paymentStarter func(context.Context, paymentInput) (paymentProcess, error)
type nodePayment struct {
	events    chan paymentEvent
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func (p *nodePayment) Events() <-chan paymentEvent { return p.events }
func (p *nodePayment) Send(v paymentCommand) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return json.NewEncoder(p.stdin).Encode(v)
}
func (p *nodePayment) Close() {
	p.closeOnce.Do(func() {
		p.stdin.Close() // EOF asks the runtime to close its browser before process termination.
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			p.cancel()
		}
	})
}
func paymentRuntimePath() string {
	if p := os.Getenv("PAYMENT_RUNTIME_PATH"); p != "" {
		return p
	}
	return filepath.Join("runtime", "payment-browser.mjs")
}
func (a *App) paymentEnabled(ctx context.Context) bool {
	if a.config(ctx, "payment")["enabled"] != true {
		return false
	}
	if a.paymentStart != nil {
		return true
	}
	if _, e := exec.LookPath("node"); e != nil {
		return false
	}
	_, e := os.Stat(paymentRuntimePath())
	if e != nil {
		return false
	}
	_, e = os.Stat(filepath.Join(filepath.Dir(paymentRuntimePath()), "node_modules", "playwright", "package.json"))
	return e == nil
}
func startPaymentNode(ctx context.Context, in paymentInput) (paymentProcess, error) {
	parent := ctx
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", paymentRuntimePath())
	cmd.Stderr = io.Discard // Browser exceptions can contain card or proxy values.
	stdin, e := cmd.StdinPipe()
	if e != nil {
		cancel()
		return nil, errors.New("runtime unavailable")
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		cancel()
		return nil, errors.New("runtime unavailable")
	}
	if e = cmd.Start(); e != nil {
		cancel()
		return nil, errors.New("runtime unavailable")
	}
	p := &nodePayment{events: make(chan paymentEvent, 16), stdin: stdin, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.events)
		defer close(p.done)
		defer cancel()
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), 16384)
		for scan.Scan() {
			var v paymentEvent
			if json.Unmarshal(scan.Bytes(), &v) != nil {
				continue
			}
			select {
			case p.events <- v:
			case <-ctx.Done():
			}
		}
		cmd.Wait()
	}()
	go func() {
		select {
		case <-parent.Done():
			p.Close()
		case <-p.done:
		}
	}()
	if e = json.NewEncoder(stdin).Encode(in); e != nil {
		p.Close()
		return nil, errors.New("runtime unavailable")
	}
	return p, nil
}
