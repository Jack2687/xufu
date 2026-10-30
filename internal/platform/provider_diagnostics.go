package platform

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"time"
)

type providerDiagnostic struct {
	Order, Phase, Code, RequestID string
	Status                        int
	Elapsed                       time.Duration
	At                            time.Time
	Details                       M
}
type providerDiagnosticKey struct{}

var safeRequestID = regexp.MustCompile(`^(?:req_[A-Za-z0-9]{8,80}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
var safeEdgeRequestID = regexp.MustCompile(`^[0-9a-fA-F]{16,32}-[A-Z]{3}$`)

func reportProvider(ctx context.Context, status int, requestID string, err error, elapsed time.Duration, details ...M) {
	if sink, ok := ctx.Value(providerDiagnosticKey{}).(func(providerDiagnostic)); ok {
		if !safeRequestID.MatchString(requestID) {
			requestID = ""
		}
		fields := M{}
		if len(details) > 0 {
			fields = safeDiagnosticDetails(details[0])
		}
		sink(providerDiagnostic{Status: status, RequestID: requestID, Code: providerCode(err), Elapsed: elapsed, At: time.Now().UTC(), Details: fields})
	}
}
func (a *App) providerContext(ctx context.Context, id, phase string) context.Context {
	return context.WithValue(ctx, providerDiagnosticKey{}, func(d providerDiagnostic) {
		d.Order, d.Phase = id, phase
		if d.Details["request_kind"] == "exit_probe" {
			d.Phase = "network"
		}
		a.enqueueDiagnostic(d)
	})
}

// An exit probe must not obscure whether a stopped payment was caused by a
// timeout, a proxy tunnel failure or an HTTP rejection. Successful probes are
// frequent during SDK resource loading, so retain only failed probe diagnostics.
func (a *App) proxyProbeContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, providerDiagnosticKey{}, func(d providerDiagnostic) {
		if d.Code == "" && d.Status < http.StatusBadRequest {
			return
		}
		d.Order, d.Phase = id, "network"
		a.enqueueDiagnostic(d)
	})
}
func (a *App) enqueueDiagnostic(d providerDiagnostic) {
	select {
	case a.providerDiagnostics <- d:
	default:
		a.diagnosticDrops.Add(1)
	}
}
func (a *App) RunProviderDiagnostics(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	reportLoss := func() {
		if drops, failures := a.diagnosticDrops.Swap(0), a.diagnosticFailures.Swap(0); drops+failures > 0 {
			log.Printf("payment diagnostics incomplete: queue_overflow=%d storage_failures=%d", drops, failures)
		}
	}
	defer reportLoss()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			reportLoss()
		case d := <-a.providerDiagnostics:
			a.saveProviderDiagnostic(d)
		}
	}
}
func (a *App) saveProviderDiagnostic(d providerDiagnostic) {
	if _, ok := paymentPhases[d.Phase]; !ok {
		return
	}
	if _, ok := traceCodes[d.Code]; !ok {
		if _, ok := paymentMessages[d.Code]; !ok {
			d.Code = "unclassified"
		}
	}
	if !safeRequestID.MatchString(d.RequestID) {
		d.RequestID = ""
	}
	if d.Status < 0 || d.Status > 599 {
		d.Status = 0
	}
	state := "completed"
	if d.Code != "" || d.Status >= http.StatusBadRequest {
		state = "failed"
	}
	if d.Code == "verification_required" || d.Details["payment_state"] == "action_required" || (d.Phase == "verification" && d.Details["sdk_outcome"] != nil && d.Details["sdk_outcome"] != "sdk_returned") {
		state = "review"
	}
	if d.Status < http.StatusBadRequest && (d.Code == "pending" || d.Code == "payment_pending" || d.Details["payment_state"] == "pending") {
		state = "review"
	}
	if d.Code == "verification_continuing" || (d.Phase == "verification" && d.Details["sdk_event"] == "progress") {
		state = "running"
	}
	if d.Phase == "verification" && d.Details["sdk_event"] == "diagnostic" {
		state = "review"
	}
	if d.Code == "subscription_absent" {
		state = "completed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := a.DB.Exec(ctx, `INSERT INTO payment_trace_events(order_id,phase,state,code,round,elapsed_ms,created_at,http_status,request_id,diagnostics)
 SELECT $1,$2,$3,$4,COALESCE((SELECT max(round) FROM payment_trace_events WHERE order_id=$1 AND created_at<=$6),1),$5,$6,$7,$8,$9
 WHERE EXISTS(SELECT 1 FROM orders WHERE id=$1)`, d.Order, d.Phase, state, d.Code, max(int64(0), d.Elapsed.Milliseconds()), d.At, d.Status, d.RequestID, safeDiagnosticDetails(d.Details))
	if err != nil {
		a.diagnosticFailures.Add(1)
	}
}
