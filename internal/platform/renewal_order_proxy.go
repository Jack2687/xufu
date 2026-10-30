package platform

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Automatic post-payment work never allocates another proxy session, including
// worker restarts and retries. The encrypted checkout binding is authoritative.
func (a *App) openOrderRenewalClient(ctx context.Context, order string) (*http.Client, func(), error) {
	if a.renewalClient != nil { // Existing private unit-test seam.
		c := a.renewalClient()
		return c, c.CloseIdleConnections, nil
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886003)"); err != nil {
		return nil, nil, err
	}
	var resource, enc, expected, workflow string
	var expires time.Time
	var enabled bool
	var capacity int
	err = tx.QueryRow(ctx, `SELECT s.resource_id,s.endpoint_enc,s.expected_ip,s.expires_at,r.enabled,r.capacity,o.workflow
 FROM order_proxy_sessions s JOIN resources r ON r.id=s.resource_id JOIN payment_runs p ON p.order_id=s.order_id
 JOIN orders o ON o.id=s.order_id
 WHERE s.order_id=$1 AND p.state='succeeded' FOR UPDATE OF s`, order).Scan(&resource, &enc, &expected, &expires, &enabled, &capacity, &workflow)
	if err == pgx.ErrNoRows || (err == nil && (enc == "" || (workflow != "subscription" && net.ParseIP(expected) == nil) || !enabled)) {
		return nil, nil, checkoutFailure("proxy_session_unavailable", false, false)
	}
	if err != nil {
		return nil, nil, err
	}
	if !expires.After(time.Now()) {
		return nil, nil, checkoutFailure("proxy_session_expired", false, false)
	}
	var used int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM proxy_leases WHERE resource_id=$1 AND expires_at>now()", resource).Scan(&used); err != nil {
		return nil, nil, err
	}
	if used >= capacity {
		return nil, nil, ErrProxyUnavailable
	}
	endpoint, err := a.decrypt(enc)
	if err != nil {
		return nil, nil, checkoutFailure("proxy_session_unavailable", false, false)
	}
	factory := newPaymentClient
	if a.renewalProxyClient != nil {
		factory = a.renewalProxyClient
	}
	client, closeTransport, err := factory(endpoint, 30*time.Second)
	if err != nil {
		return nil, nil, checkoutFailure("proxy_session_unavailable", false, false)
	}
	lease := uid()
	// Own lease: the payment worker may still be releasing its previous slot.
	if _, err = tx.Exec(ctx, "INSERT INTO proxy_leases(id,resource_id,expires_at) VALUES($1,$2,LEAST($3,now()+interval '110 seconds'))", lease, resource, expires); err != nil {
		closeTransport()
		return nil, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		closeTransport()
		return nil, nil, err
	}
	probe := *client
	client.Transport = &orderRenewalTransport{base: client.Transport, probe: &probe, expected: net.ParseIP(expected), expires: expires, skipExitIPCheck: workflow == "subscription"}
	return client, func() {
		closeTransport()
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.releaseProxy(release, lease)
	}, nil
}

// Post-payment work keeps the original endpoint and expiration. Subscription
// work no longer probes its exit; historical non-subscription orders still do.
type orderRenewalTransport struct {
	base            http.RoundTripper
	probe           *http.Client
	expected        net.IP
	expires         time.Time
	skipExitIPCheck bool
}

func (t *orderRenewalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.expires.After(time.Now()) {
		return nil, checkoutFailure("proxy_session_expired", false, false)
	}
	if t.skipExitIPCheck {
		return t.base.RoundTrip(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	v, err := providerJSON(ctx, t.probe, "GET", "https://ipinfo.io/json", http.Header{"Accept": {"application/json"}}, nil, false)
	cancel()
	if err != nil {
		return nil, checkoutFailure("proxy_unavailable", true, false)
	}
	if !t.expected.Equal(net.ParseIP(str(v, "ip"))) {
		return nil, checkoutFailure("proxy_session_changed", false, false)
	}
	if !t.expires.After(time.Now()) {
		return nil, checkoutFailure("proxy_session_expired", false, false)
	}
	return t.base.RoundTrip(r)
}

func orderRenewalProxyTerminal(err error) bool {
	switch providerCode(err) {
	case "proxy_session_unavailable", "proxy_session_expired", "proxy_session_changed":
		return true
	}
	return false
}

func waitingRenewalProxy(err error) bool { return errors.Is(err, ErrProxyUnavailable) }
