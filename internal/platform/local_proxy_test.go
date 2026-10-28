package platform

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalProxyChainsThroughConfiguredProxy(t *testing.T) {
	var outerCalls, innerCalls, targetCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "" {
			t.Error("proxy credentials leaked to target")
		}
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"authentication required"}`)
	}))
	defer origin.Close()
	tunnel := func(w http.ResponseWriter, r *http.Request, target string) {
		dst, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		src, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			t.Error(err)
			return
		}
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go func() { defer src.Close(); defer dst.Close(); io.Copy(dst, buffer) }()
		io.Copy(src, dst)
	}
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalls.Add(1)
		if r.Method != "CONNECT" || r.Host != "api.openai.com:443" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("remote:REMOTE_SECRET")) {
			t.Error("wrong configured proxy tunnel or credentials")
		}
		tunnel(w, r, origin.Listener.Addr().String())
	}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outerCalls.Add(1)
		if r.Method != "CONNECT" || r.Host != "203.0.113.44:12321" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("local:LOCAL_SECRET")) {
			t.Error("outer proxy did not connect to selected remote proxy")
		}
		tunnel(w, r, inner.Listener.Addr().String())
	}))
	defer outer.Close()
	t.Setenv("XUPAY_LOCAL_PROXY", strings.Replace(outer.URL, "http://", "http://local:LOCAL_SECRET@", 1))
	client, closeClient, err := newPaymentClient("http://remote:REMOTE_SECRET@203.0.113.44:12321", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	client.Transport.(proxyOnlyTransport).transport.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig
	// The local fixture certificate is issued for example.com, not the fixed
	// public probe hostname. Keep verification enabled with its fixture name.
	client.Transport.(proxyOnlyTransport).transport.TLSClientConfig.ServerName = "example.com"
	result := probeOpenAI(context.Background(), client)
	if result.Status != "pass" || result.HTTP != 401 || outerCalls.Load() != 1 || innerCalls.Load() != 1 || targetCalls.Load() != 1 {
		t.Fatalf("chain failed: %+v / %d,%d,%d", result, outerCalls.Load(), innerCalls.Load(), targetCalls.Load())
	}
}

func TestLocalProxyGuardsAndNoFallback(t *testing.T) {
	for _, value := range []string{"http://192.168.1.1:7897", "http://example.com:8080", "socks5://127.0.0.1:7897", "http://127.0.0.1:7897/path"} {
		t.Setenv("XUPAY_LOCAL_PROXY", value)
		if _, err := configuredProxyDialer(); err == nil {
			t.Fatal("unsafe or unsupported local proxy accepted")
		}
	}
	for _, value := range []string{"", "off"} {
		t.Setenv("XUPAY_LOCAL_PROXY", value)
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		if _, err := configuredProxyDialer(); err != nil {
			t.Fatal("production should ignore ambient proxy", err)
		}
	}
	var calls atomic.Int32
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(502) }))
	defer outer.Close()
	t.Setenv("XUPAY_LOCAL_PROXY", outer.URL)
	dial, err := configuredProxyDialer()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := dial(context.Background(), "tcp", "127.0.0.1:1234"); err == nil {
		c.Close()
		t.Fatal("private configured proxy allowed")
	}
	if calls.Load() != 0 {
		t.Fatal("private target was sent to outer proxy")
	}
	if c, err := dial(context.Background(), "tcp", "203.0.113.44:12321"); err == nil {
		c.Close()
		t.Fatal("failed outer proxy silently bypassed")
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected retry or bypass")
	}
}

func TestLocalProxyCancellationClosesHandshake(t *testing.T) {
	started := make(chan struct{})
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer outer.Close()
	t.Setenv("XUPAY_LOCAL_PROXY", outer.URL)
	dial, err := configuredProxyDialer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, e := dial(ctx, "tcp", "203.0.113.44:12321")
		if c != nil {
			c.Close()
		}
		done <- e
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled handshake remained blocked")
	}
}
