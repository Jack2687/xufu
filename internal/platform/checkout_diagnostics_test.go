package platform

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCheckoutDiagnosticsSeparateProbeAndChallenge(t *testing.T) {
	a := &App{providerDiagnostics: make(chan providerDiagnostic, 4)}
	ctx := a.providerContext(context.Background(), "test-order", "checkout")
	writes := 0
	client := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "ipinfo.io" {
			return providerResponse(200, M{"country": "PH", "ip": "203.0.113.2"}), nil
		}
		return &http.Response{StatusCode: 403, Header: http.Header{
			"Content-Type": {"text/html"}, "Cf-Mitigated": {"challenge"},
			"Cf-Ray":       {"9a1234567890abcd-HKG"},
			"X-Request-Id": {"req_1234567890"}, "Set-Cookie": {"PRIVATE_SESSION"},
		}, Body: io.NopCloser(strings.NewReader("<html>PRIVATE_RESPONSE</html>"))}, nil
	})}
	result, err := generateCheckout(ctx, client, checkoutInput{AccessToken: "PRIVATE_TOKEN", Product: "pro5x", Country: "PH", Mode: "long"}, func() error { writes++; return nil })
	if providerCode(err) != "access_denied" || result.URL != "" || writes != 1 {
		t.Fatal("rejected checkout must remain failed without retry")
	}
	probe, checkout := <-a.providerDiagnostics, <-a.providerDiagnostics
	if probe.Phase != "network" || probe.Details["request_kind"] != "exit_probe" {
		t.Errorf("IP probe was reported as checkout: phase=%s details=%v", probe.Phase, probe.Details)
	}
	if checkout.Phase != "checkout" || checkout.Details["request_kind"] != "checkout_create" || checkout.Details["challenge_required"] != true || checkout.RequestID != "req_1234567890" {
		t.Errorf("checkout challenge evidence lost: %+v", checkout)
	}
	if len(checkout.Details) != 4 || checkout.Details["response_kind"] != "html" || checkout.Details["edge_request_id"] != "9a1234567890abcd-HKG" {
		t.Fatal("unexpected diagnostic fields")
	}
}

func TestCheckoutDiagnosticValuesAreWhitelisted(t *testing.T) {
	for _, kind := range []string{"exit_probe", "checkout_create", "checkout_initialize", "session_read", "account_read", "subscription_read", "other"} {
		if safeDiagnosticDetails(M{"request_kind": kind})["request_kind"] != kind {
			t.Errorf("known request kind discarded: %s", kind)
		}
	}
	if len(safeDiagnosticDetails(M{"request_kind": "https://private.example/?token=SECRET", "challenge_required": "SECRET", "edge_request_id": "SECRET", "set_cookie": "SECRET"})) != 0 {
		t.Fatal("untrusted values entered diagnostic output")
	}
}

func TestSubscriptionCheckoutKeepsProxyWithoutExitProbe(t *testing.T) {
	h, run, _ := nativeFixture(t, false)
	guard := &protocolCheckoutRun{a: h.a, job: run.job, country: "PH"}
	for _, recorded := range []string{"", "203.0.113.9"} {
		h.a.DB.Exec(run.ctx, "UPDATE order_proxy_sessions SET expected_ip=$2 WHERE order_id=$1", run.id, recorded)
		if err := guard.beforeRequest(run.ctx, "GET", "https://chatgpt.com/"); err != nil {
			t.Fatal(err)
		}
	}
	h.a.DB.Exec(run.ctx, "UPDATE jobs SET lease_token='stale' WHERE order_id=$1", run.id)
	if err := guard.beforeRequest(run.ctx, "GET", "https://chatgpt.com/"); err == nil {
		t.Fatal("lost job lease allowed a request")
	}
}
