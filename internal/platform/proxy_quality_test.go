package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProxyOpenAIClassificationAndScores(t *testing.T) {
	for _, mode := range []string{"pass", "limited", "challenge", "challenge_html", "denied", "network", "unstable", "exit_failure"} {
		t.Run(mode, func(t *testing.T) {
			count := 0
			c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Fatal("account credential sent during proxy test")
				}
				status, body := 200, `{"ip":"203.0.113.5","country":"US"}`
				headers := http.Header{}
				if r.URL.Host == "ipinfo.io" {
					count++
					if mode == "unstable" && count == 2 {
						body = `{"ip":"203.0.113.6","country":"US"}`
					}
					if mode == "exit_failure" {
						body = `{"ip":"invalid","country":"PRIVATE_SECRET"}`
					}
				} else {
					if r.URL.String() != openAIProbeURL {
						t.Fatal("non-allowlisted probe")
					}
					status, body = 401, `{"error":"PRIVATE_RAW_RESPONSE"}`
					switch mode {
					case "limited":
						status = 429
					case "challenge":
						status = 403
						headers.Set("cf-mitigated", "challenge")
					case "challenge_html":
						status = 403
						headers.Set("Content-Type", "text/html")
						body = "<html>Cloudflare challenge</html>"
					case "denied":
						status = 403
					case "network":
						return nil, errors.New("PRIVATE_ERROR_PROXY_SECRET")
					}
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			result := measureProxy(context.Background(), c, "quality")
			expected := map[string]string{"pass": "pass", "limited": "warn", "challenge": "challenge", "challenge_html": "challenge", "denied": "fail", "network": "fail", "unstable": "pass", "exit_failure": "pass"}[mode]
			if result.OpenAI.Status != expected {
				t.Fatal(result)
			}
			if mode == "pass" && (*result.Score != 100 || result.Grade != "A") {
				t.Fatal(result)
			}
			if mode == "unstable" && (result.Stability != "changed" || *result.Score != 100 || result.Status != "pass" || result.Version != 2) {
				t.Fatal(result)
			}
			if mode == "network" && *result.Score > 70 {
				t.Fatal("failed OpenAI test earned points")
			}
			if strings.Contains(result.Message, "PRIVATE") || strings.Contains(result.OpenAI.Message, "PRIVATE") {
				t.Fatal("probe body/error leaked")
			}
		})
	}
}

func TestProxyProbeFailureDetailsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name, stage, reason string
		err                 error
	}{
		{"reset", "proxy_connect", "connection_reset", syscall.ECONNRESET},
		{"windows_reset", "proxy_connect", "connection_reset", errors.New("read tcp PRIVATE_ADDRESS: An existing connection was forcibly closed by the remote host")},
		{"tls_reset", "tls", "connection_reset", syscall.ECONNRESET},
		{"response_reset", "response", "connection_reset", syscall.ECONNRESET},
		{"timeout", "proxy_connect", "timeout", context.DeadlineExceeded},
		{"proxy_auth", "proxy_connect", "proxy_auth", errors.New("Proxy Authentication Required")},
		{"proxy_gateway", "proxy_connect", "proxy_gateway", errors.New("Bad Gateway")},
		{"closed", "proxy_connect", "connection_closed", io.EOF},
		{"unknown", "proxy_connect", "network", errors.New("PRIVATE_PROXY_PASSWORD")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(r.Context())
				if tc.stage == "tls" {
					trace.TLSHandshakeStart()
				}
				if tc.stage == "response" {
					trace.GotConn(httptrace.GotConnInfo{})
				}
				return nil, &url.Error{Op: "Get", URL: "https://PRIVATE_USER:PRIVATE_PASSWORD@PRIVATE_HOST/", Err: tc.err}
			})}
			result := probeOpenAI(context.Background(), c)
			if result.Status != "fail" || result.Stage != tc.stage || result.Reason != tc.reason || result.HTTP != 0 || result.Message == "" {
				t.Fatalf("unexpected classification: %+v", result)
			}
			b, _ := json.Marshal(result)
			if strings.Contains(string(b), "PRIVATE") {
				t.Fatal("raw diagnostic leaked")
			}
		})
	}
}

func TestProxyCheckQueuePermissionsPersistenceAndRedaction(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	resource := h.addProxy("US", 1)
	path := "/api/admin/resources/" + resource + "/checks"
	expect(t, h.request("POST", path, M{"kind": "quality"}, nil, nil), 401)
	expect(t, h.request("POST", path, M{"kind": "quality"}, h.member, nil), 403)
	expect(t, h.request("POST", path, M{"kind": "quality", "url": "https://evil.example"}, h.admin, nil), 400)
	v := expect(t, h.request("POST", path, M{"kind": "quality"}, h.admin, nil), 200)
	id := str(v, "id")
	if str(expect(t, h.request("POST", path, M{"kind": "quality"}, h.admin, nil), 200), "id") != id {
		t.Fatal("duplicate tests queued")
	}
	expect(t, h.request("POST", path, M{"kind": "openai"}, h.admin, nil), 409)
	expect(t, h.request("GET", "/api/admin/proxy-checks/"+id, nil, h.member, nil), 403)
	calls := 0
	h.a.proxyProbeClient = func(endpoint string, timeout time.Duration) (*http.Client, func(), error) {
		if !strings.Contains(endpoint, "QA_PROXY_PASSWORD") || timeout != 8*time.Second {
			t.Error("private probe configuration lost")
		}
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			body := `{"ip":"203.0.113.5","country":"US"}`
			status := 200
			if r.URL.Host == "api.openai.com" {
				status = 401
				body = `{"error":"PRIVATE_PROVIDER_RESPONSE"}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}, func() {}, nil
	}
	job, e := h.a.claimProxyCheck(ctx)
	if e != nil || job == nil || job.ID != id {
		t.Fatal(e)
	}
	if second, e := h.a.claimProxyCheck(ctx); e != nil || second != nil {
		t.Fatal("same check claimed concurrently", e)
	}
	expect(t, h.request("DELETE", "/api/admin/resources/"+resource, nil, h.admin, nil), 409)
	h.a.executeProxyCheck(ctx, job)
	if calls != 3 {
		t.Fatal("probe count", calls)
	}
	result := expect(t, h.request("GET", "/api/admin/proxy-checks/"+id, nil, h.admin, nil), 200)
	if result["state"] != "completed" {
		t.Fatal(result)
	}
	var quality, openai M
	var held int
	var ip string
	h.a.DB.QueryRow(ctx, "SELECT quality_result,openai_result,last_ip FROM resources WHERE id=$1", resource).Scan(&quality, &openai, &ip)
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM proxy_leases").Scan(&held)
	if held != 0 || num(quality, "score", -1) != 100 || str(openai, "status") != "pass" || ip != "203.0.113.5" {
		t.Fatal("probe result missing or lease retained")
	}
	for _, url := range []string{"/api/admin/proxy-checks/" + id, "/api/admin/resources", "/api/admin/audit"} {
		w := h.request("GET", url, nil, h.admin, nil)
		expect(t, w, 200)
		for _, secret := range []string{"QA_PROXY_PASSWORD", "PRIVATE_PROVIDER_RESPONSE", "endpoint_enc", "lease_token", "api.openai.com", "/v1/models"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("private data leaked", secret)
			}
		}
	}
	// Replacement credentials clear old quality data.
	expect(t, h.request("PUT", "/api/admin/resources/"+resource, M{"name": "Updated", "endpoint": "203.0.113.44:8080:user:replacement-secret", "country": "US", "enabled": true, "weight": 1, "capacity": 1}, h.admin, nil), 200)
	h.a.DB.QueryRow(ctx, "SELECT quality_result,openai_result FROM resources WHERE id=$1", resource).Scan(&quality, &openai)
	if len(quality) != 0 || len(openai) != 0 {
		t.Fatal("stale score retained after credentials changed")
	}
}

func TestProxyCheckBoundedConcurrencyAndRecovery(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id := h.addProxy("US", 1)
		expect(t, h.request("POST", "/api/admin/resources/"+id+"/checks", M{"kind": "openai"}, h.admin, nil), 200)
	}
	first, e := h.a.claimProxyCheck(ctx)
	if e != nil || first == nil {
		t.Fatal(e)
	}
	second, e := h.a.claimProxyCheck(ctx)
	if e != nil || second == nil {
		t.Fatal(e)
	}
	if third, e := h.a.claimProxyCheck(ctx); e != nil || third != nil {
		t.Fatal("unbounded probe concurrency", e)
	}
	h.a.DB.Exec(ctx, "UPDATE proxy_checks SET lease_until=now()-interval '1 second' WHERE id=$1", first.ID)
	h.a.DB.Exec(ctx, "UPDATE proxy_leases SET expires_at=now()-interval '1 second' WHERE id=$1", first.ProxyLease)
	recovered, e := h.a.claimProxyCheck(ctx)
	if e != nil || recovered == nil || recovered.ID != first.ID || recovered.Lease == first.Lease {
		t.Fatal("probe did not recover", e)
	}
	calls := 0
	h.a.proxyProbeClient = func(string, time.Duration) (*http.Client, func(), error) {
		calls++
		return nil, nil, errors.New("test")
	}
	h.a.executeProxyCheck(ctx, first)
	if calls != 0 {
		t.Fatal("stale probe ran")
	}
}
