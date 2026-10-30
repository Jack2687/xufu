package platform

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLegacyCurlDiagnosticsWithRealCurl(t *testing.T) {
	command, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("system curl not installed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("X-Request-Id", "req_1234567890")
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Cf-Ray", "9a1234567890abcd-HKG")
		w.Header().Set("Set-Cookie", "PRIVATE_SESSION")
		w.WriteHeader(http.StatusForbidden)
		w.(http.Flusher).Flush()
		io.WriteString(w, "<html>verification required</html>")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, "--disable", "--config", "-")
	cmd.Stdin = strings.NewReader("url = " + strconv.Quote(server.URL) + "\nnoproxy = \"*\"\nsilent\ndump-header = \"-\"\nsuppress-connect-headers\nwrite-out = \"\\n__XUFU_CURL_STATUS__:%{http_code}\"\n")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", server.URL, nil)
	response, err := parseCheckoutCurlResponse(req, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 403 || response.Header.Get("Cf-Mitigated") != "challenge" || response.Header.Get("X-Request-Id") != "req_1234567890" {
		t.Fatal("final response evidence was not preserved")
	}
	if len(response.Header) != 4 || response.Header.Get("Set-Cookie") != "" || response.Header.Get("Cf-Ray") != "9a1234567890abcd-HKG" {
		t.Fatal("unneeded response headers retained")
	}
	body, _ := io.ReadAll(response.Body)
	if string(body) != "<html>verification required</html>" {
		t.Fatal("headers or interim status corrupted the body")
	}
}

func TestLegacyCurlResponseRejectsTruncatedOrMismatchedOutput(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://chatgpt.com/", nil)
	for _, raw := range []string{
		"HTTP/2 403\r\n\r\nbody",
		"HTTP/2 403\r\n\r\nbody\n__XUFU_CURL_STATUS__:200",
		"body\n__XUFU_CURL_STATUS__:403",
		"HTTP/2 403\r\nBad-Header\r\n\r\nbody\n__XUFU_CURL_STATUS__:403",
		"HTTP/2 200\r\n\r\n" + strings.Repeat("x", (1<<20)+1) + "\n__XUFU_CURL_STATUS__:200",
	} {
		if _, err := parseCheckoutCurlResponse(req, raw); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	raw := "HTTP/2 200\r\nContent-Type: application/json\r\n\r\n{\"text\":\"__XUFU_CURL_STATUS__:403\"}\n__XUFU_CURL_STATUS__:200"
	response, err := parseCheckoutCurlResponse(req, raw)
	if err != nil || response.StatusCode != 200 {
		t.Fatal("body content changed final status")
	}
	response.Body.Close()
}
