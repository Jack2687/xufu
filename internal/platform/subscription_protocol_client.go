package platform

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"xupay/internal/cslive"
)

func (r *protocolRun) client(ttl int) (*cslive.Client, func(), error) {
	var enc, proxyLease string
	var expires time.Time
	var cancelStarted bool
	err := r.a.DB.QueryRow(r.ctx, `SELECT s.endpoint_enc,s.expires_at,COALESCE(c.proxy_lease_id,''),u.cancel_started
 FROM order_proxy_sessions s JOIN resources p ON p.id=s.resource_id JOIN checkout_runs c ON c.order_id=s.order_id
 JOIN subscription_runs u ON u.order_id=s.order_id WHERE s.order_id=$1 AND p.enabled`, r.id).Scan(&enc, &expires, &proxyLease, &cancelStarted)
	if err != nil || enc == "" {
		return nil, nil, cslive.Failure("proxy_session_unavailable")
	}
	if !expires.After(time.Now()) {
		return nil, nil, cslive.Failure("proxy_session_expired")
	}
	endpoint, err := r.a.decrypt(enc)
	if err != nil {
		return nil, nil, cslive.Failure("proxy_session_unavailable")
	}
	probe, closeProbe, err := newPaymentClient(endpoint, 8*time.Second)
	if err != nil {
		return nil, nil, cslive.Failure("proxy_session_unavailable")
	}
	bridge, closeBridge, err := openBrowserProxy(r.ctx, endpoint)
	if err != nil {
		closeProbe()
		return nil, nil, cslive.Failure("proxy_session_unavailable")
	}
	closeAll := func() {
		closeBridge()
		closeProbe()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.a.releaseProxy(ctx, proxyLease)
	}
	proxyURL, err := url.Parse(bridge.Server)
	if err != nil {
		closeAll()
		return nil, nil, cslive.Failure("proxy_session_unavailable")
	}
	proxyURL.User = url.UserPassword(bridge.Username, bridge.Password)
	guard := func(ctx context.Context, method, target string) error {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "https" || u.User != nil || !browserProxyHost(u.Hostname()) || (u.Port() != "" && u.Port() != "443") {
			return cslive.Failure("invalid_request_url")
		}
		if err = r.guardIP(ctx, probe); err != nil {
			return err
		}
		if method == "POST" && u.Host == "chatgpt.com" && u.Path == "/backend-api/payments/checkout" && r.job != nil {
			if err = r.a.BeforeSideEffect(ctx, r.job); err != nil {
				return cslive.Failure("checkpoint_failed")
			}
		}
		if method == "POST" && u.Path == "/backend-api/subscriptions/cancel" {
			if cancelStarted {
				return cslive.Failure("cancellation_pending")
			}
			// The checkpoint persisted cancel_started before this guard. Never
			// allow a transport retry or recovery to repeat an uncertain write.
			cancelStarted = true
		}
		return nil
	}
	timezone := map[string]string{"PH": "Asia/Manila", "US": "America/New_York", "JP": "Asia/Tokyo", "SG": "Asia/Singapore", "GB": "Europe/London", "EG": "Africa/Cairo", "TR": "Europe/Istanbul"}[r.country]
	c := cslive.NewClient(cslive.Config{Proxy: proxyURL.String(), Timeout: 45, Deadline: float64(ttl), Currency: strings.ToUpper(r.currency), Timezone: timezone}, cslive.Hooks{
		Checkpoint: r.save, BeforeRequest: guard, Progress: r.progress, Event: r.event,
	})
	return c, closeAll, nil
}

func (r *protocolRun) guardIP(ctx context.Context, probe *http.Client) error {
	var expected string
	var expires time.Time
	var enabled bool
	err := r.a.DB.QueryRow(ctx, `SELECT s.expected_ip,s.expires_at,p.enabled FROM order_proxy_sessions s
 JOIN resources p ON p.id=s.resource_id JOIN subscription_protocol c ON c.order_id=s.order_id
 JOIN checkout_runs j ON j.order_id=s.order_id JOIN proxy_leases l ON l.id=j.proxy_lease_id AND l.expires_at>now()
 WHERE s.order_id=$1 AND c.lease_token=$2 AND c.lease_until>now() AND c.expires_at>now()`, r.id, r.lease).Scan(&expected, &expires, &enabled)
	if err != nil || !enabled {
		return cslive.Failure("proxy_session_unavailable")
	}
	if !expires.After(time.Now()) {
		return cslive.Failure("proxy_session_expired")
	}
	check, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	v, err := providerJSON(check, probe, "GET", "https://ipinfo.io/json", http.Header{"Accept": {"application/json"}}, nil, false)
	if err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	ip := net.ParseIP(str(v, "ip"))
	if ip == nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	if expected == "" {
		if r.job == nil {
			return cslive.Failure("proxy_session_unavailable")
		}
		if !strings.EqualFold(str(v, "country"), r.country) {
			return cslive.Failure("proxy_country_mismatch")
		}
		if err = r.a.recordOrderProxyIP(ctx, r.job, ip.String()); err != nil {
			return cslive.Failure("proxy_session_changed")
		}
		r.a.paymentTrace(ctx, r.id, "network", "completed", "", 0)
	} else if !net.ParseIP(expected).Equal(ip) {
		return cslive.Failure("proxy_session_changed")
	}
	if !expires.After(time.Now()) {
		return cslive.Failure("proxy_session_expired")
	}
	var valid bool
	if r.a.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_protocol s JOIN checkout_runs c ON c.order_id=s.order_id
 JOIN proxy_leases l ON l.id=c.proxy_lease_id JOIN resources p ON p.id=c.resource_id
 WHERE s.order_id=$1 AND s.lease_token=$2 AND s.lease_until>now() AND s.expires_at>now() AND l.expires_at>now() AND p.enabled)`, r.id, r.lease).Scan(&valid) != nil || !valid {
		return cslive.Failure("proxy_session_unavailable")
	}
	return nil
}

func protocolPublicCode(codes ...string) string {
	for _, code := range codes {
		if code == "" {
			continue
		}
		if _, ok := traceCodes[code]; ok {
			return code
		}
		if _, ok := paymentMessages[code]; ok {
			return code
		}
		switch code {
		case "http_401", "session_expired", "session_token_required":
			return "invalid_session"
		case "http_403", "approval_blocked":
			return "access_denied"
		case "network_error", "network_timeout", "deadline_exceeded":
			return "network"
		case "checkpoint_failed":
			return "internal_error"
		case "not_charged":
			return "payment_declined"
		case "action_required":
			return "verification_required"
		case "cancellation_pending", "renewal_state_unknown":
			return "review"
		default:
			return "invalid_response"
		}
	}
	return ""
}

func (r *protocolRun) progress(p cslive.Progress) {
	phase := map[string]string{"session": "account_check", "checkout": "checkout", "payment": "confirming", "cancellation": "renewal"}[string(p.Stage)]
	state := map[string]string{"running": "running", "succeeded": "completed", "failed": "failed", "pending": "review"}[string(p.Status)]
	if r.stageStarted == nil {
		r.stageStarted = map[string]time.Time{}
	}
	if state == "running" {
		r.stageStarted[phase] = time.Now()
	}
	var elapsed time.Duration
	if started, ok := r.stageStarted[phase]; ok {
		elapsed = time.Since(started)
	}
	r.a.paymentTrace(r.ctx, r.id, phase, state, protocolPublicCode(p.Code), elapsed)
}

func (r *protocolRun) event(event map[string]any) {
	if str(event, "event") != "http_request" && str(event, "event") != "payment_error" {
		return
	}
	phase := protocolDiagnosticPhase(str(event, "path"), r.last.Phase)
	requestID := str(event, "requestId")
	if requestID == "" {
		requestID = str(event, "responseRequestId")
	}
	if !safeRequestID.MatchString(requestID) {
		requestID = ""
	}
	details := M{"protocol_code": str(event, "errorCode"), "provider_code": str(event, "providerCode"), "decline_code": str(event, "declineCode"), "request_attempt": event["attempt"], "network_stage": str(event, "networkStage")}
	for source, dest := range map[string]string{"connectionReadyMs": "connection_ready_ms", "requestWriteMs": "request_write_ms", "firstByteMs": "first_byte_ms", "responseWaitMs": "response_wait_ms", "networkStageElapsedMs": "network_stage_elapsed_ms"} {
		if v, ok := event[source]; ok {
			details[dest] = v
		}
	}
	r.a.enqueueDiagnostic(providerDiagnostic{Order: r.id, Phase: phase, Code: protocolPublicCode(str(event, "errorCode")), Status: int(protocolEventNumber(event["status"])), RequestID: requestID,
		Elapsed: time.Duration(protocolEventNumber(event["durationMs"])) * time.Millisecond, At: time.Now().UTC(), Details: safeDiagnosticDetails(details)})
}

func protocolEventNumber(value any) int64 {
	switch n := value.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		if n >= 0 && n <= 3600000 {
			return int64(n)
		}
	}
	return 0
}

// Paths are inspected only in memory; neither the path nor its parameters enter
// the diagnostic record or any customer/admin response.
func protocolDiagnosticPhase(path, phase string) string {
	switch {
	case strings.Contains(path, "/api/auth/session"):
		return "session_refresh"
	case strings.Contains(path, "/accounts/check/"):
		return "account_check"
	case strings.Contains(path, "/subscriptions/cancel"):
		return "renewal"
	case strings.Contains(path, "/subscriptions"):
		if phase == "activation" {
			return "activation"
		}
		if strings.HasPrefix(phase, "cancellation") {
			return "renewal"
		}
		return "account_check"
	case strings.Contains(path, "tax"), strings.HasSuffix(path, "/init"):
		return "pricing"
	case strings.Contains(path, "payment_methods"), strings.Contains(path, "confirmation_tokens"):
		return "billing"
	case strings.Contains(path, "/confirm"), strings.Contains(path, "/approve"), strings.Contains(path, "subscription/update"):
		return "confirming"
	case strings.Contains(path, "payment_intents"), strings.Contains(path, "/poll"):
		return "checking"
	}
	switch phase {
	case "activation":
		return "activation"
	case "cancellation", "cancellation_requested":
		return "renewal"
	case "payment_result":
		return "checking"
	case "hosted_confirm", "stripe_confirm", "openai_confirm_checkout", "subscription_update":
		return "confirming"
	default:
		return "checkout"
	}
}
