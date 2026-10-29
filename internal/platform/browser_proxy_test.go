package platform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserProxyBoundaries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	p, closeProxy, e := startBrowserProxy(ctx, func(context.Context, string) (net.Conn, error) {
		calls.Add(1)
		a, b := net.Pipe()
		go func() { defer b.Close(); io.Copy(b, b) }()
		return a, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	defer closeProxy()
	u, _ := url.Parse(p.Server)
	for _, tc := range []struct {
		host, auth string
		want       int
	}{{"pay.openai.com:443", "", 407}, {"evil.example:443", p.Password, 403}, {"pay.openai.com:80", p.Password, 403}, {"challenges.cloudflare.com:443", "", 407}, {"challenges.cloudflare.com.evil.example:443", p.Password, 403}, {"other.cloudflare.com:443", p.Password, 403}, {"challenges.cloudflare.com:80", p.Password, 403}, {"pay.openai.com:443", p.Password, 200}, {"challenges.cloudflare.com:443", p.Password, 200}} {
		c, e := net.Dial("tcp", u.Host)
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		req := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: tc.host}, Host: tc.host, Header: http.Header{}}
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.Username+":"+tc.auth)))
		req.Write(c)
		reader := bufio.NewReader(c)
		res, e := http.ReadResponse(reader, req)
		if e != nil || res.StatusCode != tc.want {
			t.Fatalf("CONNECT boundary: %v %v", res, e)
		}
		if tc.want == 200 {
			c.Write([]byte("opaque tls bytes"))
			buf := make([]byte, 16)
			if _, e = io.ReadFull(reader, buf); e != nil || string(buf) != "opaque tls bytes" {
				t.Fatal("tunnel altered bytes", e)
			}
			if tc.host == "challenges.cloudflare.com:443" {
				cancel()
				if _, e = reader.ReadByte(); e == nil {
					t.Fatal("cancel left tunnel open")
				}
			}
		}
		c.Close()
	}
	if calls.Load() != 2 {
		t.Fatal("blocked target reached supplier")
	}
}

// Real Chromium page traffic and its APIRequestContext traverse the production
// CONNECT bridge and the same authenticated supplier mock. No external traffic.
func TestChromiumUsesPinnedProxyForPageAndProbe(t *testing.T) {
	if _, e := exec.LookPath("node"); e != nil {
		t.Skip("node required")
	}
	var page, probe, connects atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			probe.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ip":"203.0.113.2"}`)
		} else {
			page.Add(1)
			io.WriteString(w, "<html><body>proxy verified</body></html>")
		}
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("supplier:secret_session-fixed")) {
			t.Error("supplier session not forwarded")
			w.WriteHeader(407)
			return
		}
		connects.Add(1)
		up, e := net.Dial("tcp", originURL.Host)
		if e != nil {
			t.Error(e)
			w.WriteHeader(502)
			return
		}
		defer up.Close()
		down, rw, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		defer down.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		done := make(chan struct{})
		go func() { io.Copy(up, rw); up.Close(); close(done) }()
		io.Copy(down, up)
		down.Close()
		up.Close()
		<-done
	}))
	defer gateway.Close()
	gatewayURL, _ := url.Parse(gateway.URL)
	upstream, _ := url.Parse("http://supplier:secret_session-fixed@geo.iproyal.com:12321")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bridge, closeBridge, e := startBrowserProxy(ctx, func(ctx context.Context, target string) (net.Conn, error) {
		return upstreamProxyTunnel(ctx, upstream, target, func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != upstream.Host {
				t.Error("supplier bypassed")
			}
			return (&net.Dialer{}).DialContext(ctx, network, gatewayURL.Host)
		})
	})
	if e != nil {
		t.Fatal(e)
	}
	defer closeBridge()
	input, _ := json.Marshal(bridge)
	script := `import {chromium} from 'playwright';import fs from 'node:fs';const p=JSON.parse(fs.readFileSync(0,'utf8'));let b;try{b=await chromium.launch({headless:true,proxy:p,executablePath:process.env.PAYMENT_CHROME_PATH||(process.platform==='win32'?'C:/Program Files/Google/Chrome/Application/chrome.exe':undefined)});const c=await b.newContext({proxy:p,ignoreHTTPSErrors:true});const r=await c.request.get('https://ipinfo.io/probe',{timeout:8000});if((await r.json()).ip!=='203.0.113.2')throw Error('probe failed');const page=await c.newPage();await page.goto('https://pay.openai.com/test',{timeout:8000});if(!(await page.textContent('body')).includes('proxy verified'))throw Error('page failed');console.log('verified')}finally{await b?.close()}`
	cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script)
	cmd.Dir = "../../runtime"
	cmd.Stdin = bytes.NewReader(input)
	output, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("real proxy smoke: %v %s", e, output)
	}
	if !strings.Contains(string(output), "verified") || page.Load() < 1 || probe.Load() != 1 || connects.Load() < 2 {
		t.Fatalf("missing traffic page=%d probe=%d tunnels=%d", page.Load(), probe.Load(), connects.Load())
	}
}
