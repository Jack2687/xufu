package platform

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	socks "golang.org/x/net/proxy"
)

type browserProxyInput struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}
type contextProxyDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d contextProxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.dial(context.Background(), network, address)
}
func (d contextProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

func browserProxyHost(host string) bool {
	for _, h := range []string{"pay.openai.com", "checkout.stripe.com", "chatgpt.com", "cdn.oaistatic.com", "tcr9i.chat.openai.com", "challenges.cloudflare.com", "api.stripe.com", "js.stripe.com", "stripe.com", "stripe.network", "b.stripecdn.com", "hcaptcha.com", "ipinfo.io", "stripe-camo.global.ssl.fastly.net", "d1wqzb5bdbcre6.cloudfront.net", "maps.googleapis.com", "applepay.cdn-apple.com"} {
		if host == h {
			return true
		}
	}
	for _, h := range []string{"stripe.com", "stripe.network", "hcaptcha.com"} {
		if strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// CONNECT relay preserves end-to-end TLS. It is private to one payment runtime,
// accepts only approved HTTPS hosts, and never falls back to a direct connection.
func openBrowserProxy(ctx context.Context, endpoint string) (*browserProxyInput, func(), error) {
	raw, err := normalizeProxy(endpoint)
	if err != nil {
		return nil, nil, errors.New("proxy unavailable")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, errors.New("proxy unavailable")
	}
	dial, err := configuredProxyDialer()
	if err != nil {
		return nil, nil, err
	}
	return startBrowserProxy(ctx, func(c context.Context, target string) (net.Conn, error) {
		return upstreamProxyTunnel(c, u, target, dial)
	})
}
func upstreamProxyTunnel(ctx context.Context, u *url.URL, target string, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		var auth *socks.Auth
		if u.User != nil {
			p, _ := u.User.Password()
			auth = &socks.Auth{User: u.User.Username(), Password: p}
		}
		d, err := socks.SOCKS5("tcp", u.Host, auth, contextProxyDialer{dial})
		if err != nil {
			return nil, err
		}
		return d.(socks.ContextDialer).DialContext(ctx, "tcp", target)
	}
	c, err := dial(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	if d, yes := ctx.Deadline(); yes && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	if u.Scheme == "https" {
		tc := tls.Client(c, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err = tc.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		c = tc
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	if u.User != nil {
		p, _ := u.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+p)))
	}
	if err = req.Write(c); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(c)
	res, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, errors.New("proxy tunnel rejected")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.SetDeadline(time.Time{})
	ok = true
	return &proxyBufferedConn{Conn: c, reader: reader}, nil
}
func startBrowserProxy(parent context.Context, tunnel func(context.Context, string) (net.Conn, error)) (*browserProxyInput, func(), error) {
	return startBrowserProxyChecked(parent, tunnel, func(_ context.Context, host string) bool { return browserProxyHost(host) })
}

// The official SDK may navigate a bank's public HTTPS verification page.
// This private relay remains bound to the original order's provider session.
func startBrowserProxyChecked(parent context.Context, tunnel func(context.Context, string) (net.Conn, error), allow func(context.Context, string) bool) (*browserProxyInput, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		return nil, nil, err
	}
	cfg := &browserProxyInput{Server: "http://" + ln.Addr().String(), Username: "order", Password: secret()}
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.Username+":"+cfg.Password))
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16384}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(expected)) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="order"`)
			w.WriteHeader(407)
			return
		}
		host, port, e := net.SplitHostPort(r.Host)
		if r.Method != http.MethodConnect || e != nil || port != "443" {
			w.WriteHeader(403)
			return
		}
		cctx, stop := context.WithTimeout(ctx, 20*time.Second)
		if !allow(cctx, strings.ToLower(host)) {
			stop()
			w.WriteHeader(403)
			return
		}
		up, e := tunnel(cctx, net.JoinHostPort(host, port))
		stop()
		if e != nil {
			w.WriteHeader(502)
			return
		}
		hijack, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			w.WriteHeader(500)
			return
		}
		downstream, rw, e := hijack.Hijack()
		if e != nil {
			up.Close()
			return
		}
		defer downstream.Close()
		defer up.Close()
		closeOnCancel := context.AfterFunc(ctx, func() { downstream.Close(); up.Close() })
		defer closeOnCancel()
		if _, e = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
			return
		}
		if rw.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(up, rw); up.Close(); close(done) }()
		io.Copy(downstream, up)
		downstream.Close()
		up.Close()
		<-done
	})
	go server.Serve(ln)
	var once sync.Once
	closeFn := func() { once.Do(func() { cancel(); server.Close() }) }
	go func() { <-ctx.Done(); server.Close() }()
	return cfg, closeFn, nil
}
