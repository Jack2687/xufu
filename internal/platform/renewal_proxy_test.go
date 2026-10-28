package platform

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func chooseGlobal(t *testing.T, h *harness, id string) {
	t.Helper()
	expect(t, h.request("POST", "/api/admin/resources/"+id+"/global", M{}, h.admin, nil), 200)
}

func TestRenewalGlobalProxySelectionAndCapacity(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	regional := h.addProxy("PH", 3)
	expect(t, h.request("PUT", "/api/admin/settings/proxy", M{"default_id": regional, "match_country": true, "request_timeout_seconds": 60}, h.admin, nil), 200)
	if c, _, e := h.a.openRenewalClient(ctx); !errors.Is(e, ErrProxyUnavailable) || c != nil {
		t.Fatal("regional fallback or direct transport used", e)
	}
	global := h.addProxy("US", 1)
	chooseGlobal(t, h, global)
	client, release, e := h.a.openRenewalClient(ctx)
	if e != nil {
		t.Fatal(e)
	}
	tr := client.Transport.(proxyOnlyTransport).transport
	req, _ := http.NewRequest("GET", subscriptionEndpoint, nil)
	proxy, e := tr.Proxy(req)
	if e != nil || proxy == nil || client.Timeout != 30*time.Second {
		t.Fatal("missing forced proxy transport")
	}
	if c, _, e := h.a.openRenewalClient(ctx); !errors.Is(e, ErrProxyUnavailable) || c != nil {
		t.Fatal("capacity limit bypassed")
	}
	expect(t, h.request("DELETE", "/api/admin/resources/"+global, nil, h.admin, nil), 409)
	// Switching configuration keeps the old lease/client bound until its round completes.
	chooseGlobal(t, h, regional)
	var held string
	h.a.DB.QueryRow(ctx, "SELECT resource_id FROM proxy_leases WHERE resource_id=$1", global).Scan(&held)
	if held != global {
		t.Fatal("in-flight proxy lease changed")
	}
	release()
	// Old configuration editors must preserve the global selection.
	expect(t, h.request("PUT", "/api/admin/settings/proxy", M{"default_id": "", "match_country": true, "request_timeout_seconds": 60}, h.admin, nil), 200)
	if str(h.a.config(ctx, "proxy"), "global_id") != regional {
		t.Fatal("legacy save erased global proxy")
	}
	h.a.DB.Exec(ctx, "UPDATE resources SET enabled=false WHERE id=$1", regional)
	if _, _, e = h.a.openRenewalClient(ctx); !errors.Is(e, ErrProxyUnavailable) {
		t.Fatal("disabled global proxy used")
	}
	expect(t, h.request("POST", "/api/admin/resources/"+regional+"/global", M{}, h.admin, nil), 400)
	expect(t, h.request("POST", "/api/admin/resources/"+global+"/global", M{}, h.member, nil), 403)
	expect(t, h.request("POST", "/api/admin/resources/"+global+"/global", M{}, nil, nil), 401)
	expect(t, h.request("DELETE", "/api/admin/resources/"+regional, nil, h.admin, nil), 200)
	if str(h.a.config(ctx, "proxy"), "global_id") != "" {
		t.Fatal("deleted global selection retained")
	}
}

func TestRenewalWaitsForGlobalWithoutConsumingChecks(t *testing.T) {
	for _, mode := range []string{"standalone", "subscription"} {
		t.Run(mode, func(t *testing.T) {
			var h *harness
			var id string
			if mode == "standalone" {
				h = setupTest(t)
				id = newStandalone(t, h, "renewal@example.com")
			} else {
				var p *fakePayment
				h, id, p = subscriptionFixture(t, false)
				completeSubscriptionCheckout(t, h, id)
				p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
				waitPayment(t, h, id, "submitting")
				p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
				waitPayment(t, h, id, "succeeded")
			}
			ctx := context.Background()
			claim, run := h.a.claimRenewal, h.a.runRenewal
			table, idcol := "subscription_runs", "order_id"
			if mode == "standalone" {
				claim, run = h.a.claimStandaloneRenewal, h.a.runStandaloneRenewal
				table, idcol = "renewal_tasks", "id"
			}
			for i := 0; i < 2; i++ {
				h.a.DB.Exec(ctx, "UPDATE "+table+" SET next_at=now() WHERE "+idcol+"=$1", id)
				next, lease, e := claim(ctx)
				if e != nil || next != id {
					t.Fatal("claim failed", e)
				}
				run(ctx, id, lease)
				var checks int
				var started, waiting, released bool
				h.a.DB.QueryRow(ctx, "SELECT checks,cancel_started,next_at>now(),lease_token IS NULL FROM "+table+" WHERE "+idcol+"=$1", id).Scan(&checks, &started, &waiting, &released)
				if checks != 0 || started || !waiting || !released {
					t.Fatal("waiting consumed checks or retained lease", checks, started, waiting, released)
				}
			}
		})
	}
}

// Real local CONNECT/TLS transport verifies GET, cancellation POST and confirming
// GET all traverse the selected proxy. No public upstream receives these requests.
func TestBothRenewalFlowsUseGlobalProxyForEveryRequest(t *testing.T) {
	for _, mode := range []string{"standalone", "subscription"} {
		t.Run(mode, func(t *testing.T) {
			var h *harness
			var id string
			if mode == "standalone" {
				h = setupTest(t)
				id = newStandalone(t, h, "renewal@example.com")
			} else {
				var p *fakePayment
				h, id, p = subscriptionFixture(t, false)
				completeSubscriptionCheckout(t, h, id)
				p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
				waitPayment(t, h, id, "submitting")
				p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
				waitPayment(t, h, id, "succeeded")
			}
			ctx := context.Background()
			global := h.addProxy("US", 1)
			chooseGlobal(t, h, global)
			endpoint := "http://test-user:TEST_PROXY_SECRET@proxy.example.test:8080"
			enc, _ := h.a.encrypt(endpoint)
			h.a.DB.Exec(ctx, "UPDATE resources SET endpoint_enc=$2 WHERE id=$1", global, enc)
			var writes, reads, tunnels atomic.Int32
			var mu sync.Mutex
			methods := []string{}
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("Chatgpt-Account-Id") != "test-space" {
					t.Error("incorrect header isolation")
				}
				mu.Lock()
				methods = append(methods, r.Method)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "POST" {
					writes.Add(1)
					io.WriteString(w, `{"success":true}`)
					return
				}
				reads.Add(1)
				if writes.Load() > 0 {
					io.WriteString(w, `{"is_active":true,"will_renew":false}`)
				} else {
					io.WriteString(w, `{"is_active":true,"will_renew":true}`)
				}
			}))
			defer upstream.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "CONNECT" || r.Host != "chatgpt.com:443" {
					t.Error("did not use proxy CONNECT")
				}
				if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("test-user:TEST_PROXY_SECRET")) {
					t.Error("missing proxy authentication")
				}
				dst, e := net.Dial("tcp", upstream.Listener.Addr().String())
				if e != nil {
					t.Error(e)
					w.WriteHeader(502)
					return
				}
				src, buf, e := w.(http.Hijacker).Hijack()
				if e != nil {
					dst.Close()
					t.Error(e)
					return
				}
				tunnels.Add(1)
				buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				buf.Flush()
				go func() { defer src.Close(); defer dst.Close(); io.Copy(dst, src) }()
				io.Copy(src, dst)
			}))
			defer proxy.Close()
			factories := 0
			h.a.renewalProxyClient = func(selected string, timeout time.Duration) (*http.Client, func(), error) {
				factories++
				if selected != endpoint {
					t.Error("wrong global proxy")
				}
				c, closeClient, e := newPaymentClient(selected, timeout)
				if e != nil {
					return c, closeClient, e
				}
				tr := c.Transport.(proxyOnlyTransport).transport
				// Test-only TLS server and local dial preserve the real proxy CONNECT behavior.
				tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
				tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					if address != "proxy.example.test:8080" {
						t.Error("direct or alternate dial", address)
					}
					return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
				}
				return c, closeClient, nil
			}
			if mode == "standalone" {
				runStandalone(t, h, id)
				standaloneStatus(t, h, id, "disabled")
			} else {
				next, lease, e := h.a.claimRenewal(ctx)
				if e != nil || next != id {
					t.Fatal(e)
				}
				h.a.runRenewal(ctx, id, lease)
				var state string
				h.a.DB.QueryRow(ctx, "SELECT renewal FROM orders WHERE id=$1", id).Scan(&state)
				if state != "disabled" {
					t.Fatal(state)
				}
			}
			if writes.Load() != 1 || reads.Load() != 2 || tunnels.Load() < 1 || factories != 1 {
				t.Fatal("flow did not use one proxied session", writes.Load(), reads.Load(), tunnels.Load(), factories)
			}
			mu.Lock()
			joined := strings.Join(methods, ",")
			mu.Unlock()
			if joined != "GET,POST,GET" {
				t.Fatal(joined)
			}
			var leases int
			h.a.DB.QueryRow(ctx, "SELECT count(*) FROM proxy_leases WHERE resource_id=$1", global).Scan(&leases)
			if leases != 0 {
				t.Fatal("global lease not released")
			}
		})
	}
}
