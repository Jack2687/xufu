package platform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type checkoutCurlTransport struct {
	command, proxy string
	timeout        time.Duration
}

// The supplied generator uses system curl. Keep credentials in stdin, and use
// the existing private tunnel to retain the configured proxy and Windows chain.
func newLegacyCheckoutClient(ctx context.Context, endpoint string) (*http.Client, func(), error) {
	command, err := exec.LookPath("curl.exe")
	if err != nil {
		command, err = exec.LookPath("curl")
	}
	if err != nil {
		return nil, nil, checkoutFailure("checkout_runtime_unavailable", false, false)
	}
	bridge, closeBridge, err := openBrowserProxy(ctx, endpoint)
	if err != nil {
		return nil, nil, checkoutFailure("proxy_unavailable", false, false)
	}
	proxy, err := url.Parse(bridge.Server)
	if err != nil {
		closeBridge()
		return nil, nil, checkoutFailure("proxy_unavailable", false, false)
	}
	proxy.User = url.UserPassword(bridge.Username, bridge.Password)
	return &http.Client{
		Transport:     checkoutCurlTransport{command: command, proxy: proxy.String(), timeout: 60 * time.Second},
		Timeout:       65 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, closeBridge, nil
}

type checkoutCurlOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *checkoutCurlOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) + (16 << 10) - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:max(remaining, 0)]
	}
	b.Buffer.Write(p)
	return n, nil
}

func (t checkoutCurlTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.User != nil || (r.URL.Port() != "" && r.URL.Port() != "443") {
		return nil, errors.New("upstream target denied")
	}
	switch r.URL.Hostname() {
	case "chatgpt.com", "api.stripe.com", "ipinfo.io":
	default:
		return nil, errors.New("upstream target denied")
	}
	if t.proxy == "" || (r.Method != "GET" && r.Method != "POST") {
		return nil, errors.New("invalid checkout transport")
	}
	var config strings.Builder
	option := func(key, value string) {
		config.WriteString(key + " = " + strconv.Quote(value) + "\n")
	}
	option("url", r.URL.String())
	option("request", r.Method)
	option("proxy", t.proxy)
	option("noproxy", "")
	option("proto", "=https")
	option("max-time", strconv.FormatFloat(t.timeout.Seconds(), 'f', 0, 64))
	option("connect-timeout", "10")
	option("write-out", "\n__XUFU_CURL_STATUS__:%{http_code}")
	option("dump-header", "-")
	config.WriteString("silent\nshow-error\nsuppress-connect-headers\n")
	for key, values := range r.Header {
		for _, value := range values {
			if strings.ContainsAny(key+value, "\r\n\x00") {
				return nil, errors.New("invalid checkout header")
			}
			option("header", key+": "+value)
		}
	}
	if r.Body != nil {
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			return nil, errors.New("invalid checkout body")
		}
		option("data-binary", string(body))
	}
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout+5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, t.command, "--disable", "--config", "-")
	command.Stdin = strings.NewReader(config.String())
	command.WaitDelay = 2 * time.Second
	var stdout checkoutCurlOutput
	command.Stdout, command.Stderr = &stdout, io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("checkout curl transport failed")
	}
	if stdout.overflow {
		return nil, errors.New("checkout response too large")
	}
	return parseCheckoutCurlResponse(r, stdout.String())
}
