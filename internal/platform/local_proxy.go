package platform

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// A local launcher may opt into chaining configured proxies through a desktop
// HTTP proxy. Production does not infer this setting from HTTP_PROXY/HTTPS_PROXY.
// This is process-only configuration, never a customer-controlled resource URL.
func configuredProxyDialer() (func(context.Context, string, string) (net.Conn, error), error) {
	raw := strings.TrimSpace(os.Getenv("XUPAY_LOCAL_PROXY"))
	if raw == "" || strings.EqualFold(raw, "off") {
		return publicDial, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("本地代理链配置无效，需要本机 HTTP 代理")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("本地代理链仅允许回环地址")
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, errors.New("unsupported proxy network")
		}
		// Preserve the same public-resource validation as direct dialing. Pin
		// the validated address so the outer proxy cannot resolve it differently.
		target, err := publicDialAddress(ctx, address)
		if err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: 8 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, fmt.Errorf("local proxy connection: %w", err)
		}
		ok := false
		defer func() {
			if !ok {
				conn.Close()
			}
		}()
		deadline := time.Now().Add(8 * time.Second)
		if d, has := ctx.Deadline(); has && d.Before(deadline) {
			deadline = d
		}
		conn.SetDeadline(deadline)
		cancelled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { conn.Close(); close(cancelled) })
		defer stop()
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
		if u.User != nil {
			password, _ := u.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
		}
		if err = req.Write(conn); err != nil {
			return nil, fmt.Errorf("local proxy tunnel: %w", err)
		}
		reader := bufio.NewReader(conn)
		res, err := http.ReadResponse(reader, req)
		if err != nil {
			return nil, fmt.Errorf("local proxy tunnel: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("local proxy tunnel returned HTTP %d", res.StatusCode)
		}
		if !stop() {
			<-cancelled
			return nil, ctx.Err()
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if err = conn.SetDeadline(time.Time{}); err != nil {
			return nil, err
		}
		ok = true
		return &proxyBufferedConn{Conn: conn, reader: reader}, nil
	}, nil
}

type proxyBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *proxyBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
