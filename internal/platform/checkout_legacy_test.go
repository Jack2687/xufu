package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLegacyScriptHostedFallback(t *testing.T) {
	for _, mode := range []string{"returned_key", "fallback_key", "short", "missing_url", "missing_success", "custom", "mismatched", "invalid_id"} {
		t.Run(mode, func(t *testing.T) {
			writes, calls, inits := 0, 0, 0
			in := checkoutInput{AccessToken: "PRIVATE_AT", Product: "pro5x", Country: "PH", Mode: "long"}
			client := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.URL.Hostname() {
				case "ipinfo.io":
					return providerResponse(200, M{"country": "PH", "ip": "203.0.113.2"}), nil
				case "chatgpt.com":
					if r.URL.String() != checkoutEndpoint || r.Method != "POST" || r.Header.Get("OpenAI-Client-Type") != "native" || r.Header.Get("OAI-Language") != "zh-Hans" || r.Header.Get("OpenAI-Client-Name") != "chatgpt-ios" {
						t.Fatal("script request mismatch")
					}
					result := M{"checkout_session_id": "cs_live_Test", "checkout_ui_mode": "hosted"}
					if mode == "returned_key" {
						result["publishable_key"] = "pk_test_returned"
					}
					if mode == "invalid_id" {
						result["checkout_session_id"] = "oaics_Test"
					}
					return providerResponse(200, result), nil
				case "api.stripe.com":
					inits++
					if r.Method != "POST" || r.URL.Path != "/v1/payment_pages/cs_live_Test/init" || r.Header.Get("Authorization") != "" {
						t.Fatal("unsafe init")
					}
					body, _ := io.ReadAll(r.Body)
					form, _ := url.ParseQuery(string(body))
					wantKey := legacyCheckoutPublishableKey
					if mode == "returned_key" {
						wantKey = "pk_test_returned"
					}
					if form.Get("key") != wantKey || form.Get("eid") != "NA" || form.Get("browser_locale") != "en-US" || form.Get("browser_timezone") != "Asia/Manila" || form.Get("redirect_type") != "url" {
						t.Fatal("script init mismatch")
					}
					result := M{"ui_mode": "hosted", "success_url": "https://chatgpt.com/", "stripe_hosted_url": "https://checkout.stripe.com/c/pay/cs_live_Test#real-fragment"}
					switch mode {
					case "short":
						result["stripe_hosted_url"] = "https://chatgpt.com/checkout/openai_llc/cs_live_Test"
					case "missing_url":
						delete(result, "stripe_hosted_url")
					case "missing_success":
						delete(result, "success_url")
					case "custom":
						result["ui_mode"] = "custom"
					case "mismatched":
						result["stripe_hosted_url"] = "https://pay.openai.com/c/pay/cs_live_Other"
					}
					return providerResponse(200, result), nil
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})}
			out, err := generateCheckout(context.Background(), client, in, func() error { writes++; return nil })
			if mode == "returned_key" || mode == "fallback_key" {
				if err != nil || out.URL != "https://pay.openai.com/c/pay/cs_live_Test#real-fragment" || calls != 3 || writes != 1 || inits != 1 {
					t.Fatal("script fallback failed", err)
				}
			} else {
				var failure checkoutError
				if !errors.As(err, &failure) || failure.Retry || out.URL != "" || !failure.Unknown {
					t.Fatal("unsafe result", err)
				}
			}
		})
	}
}

func TestLegacyCurlEnforcesProxyAndTarget(t *testing.T) {
	t.Setenv("XUPAY_LOCAL_PROXY", "")
	command, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("system curl not installed")
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "CONNECT" || r.Host != "chatgpt.com:443" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Error("wrong proxy tunnel")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("token leaked to proxy")
		}
		w.WriteHeader(403)
	}))
	defer proxy.Close()
	endpoint := strings.Replace(proxy.URL, "http://", "http://user:pass@", 1)
	client := &http.Client{Transport: checkoutCurlTransport{command: command, proxy: endpoint, timeout: time.Second}}
	// Even a process-wide bypass must not bypass the configured order proxy.
	t.Setenv("NO_PROXY", "*")
	r, _ := http.NewRequest("POST", checkoutEndpoint, strings.NewReader(`{"plan_name":"chatgptprolite"}`))
	r.Header.Set("Authorization", "Bearer PRIVATE_TEST_TOKEN")
	_, err = client.Do(r)
	if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "PRIVATE_TEST_TOKEN") || strings.Contains(err.Error(), "user:pass") {
		t.Fatal("curl failed proxy enforcement or leaked credentials")
	}
	for _, target := range []string{"https://unapproved.example/", "http://chatgpt.com/", "https://chatgpt.com:444/"} {
		if _, err = client.Get(target); err == nil {
			t.Fatal("unapproved target allowed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ = http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/", nil)
	started := time.Now()
	if _, err = client.Do(r); err == nil || time.Since(started) > 2*time.Second || calls.Load() != 1 {
		t.Fatal("cancel or target guard failed")
	}
}
