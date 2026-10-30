package platform

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

func parseCheckoutCurlResponse(req *http.Request, raw string) (*http.Response, error) {
	const separator = "\n__XUFU_CURL_STATUS__:"
	index := strings.LastIndex(raw, separator)
	invalid := errors.New("checkout response invalid")
	if index < 0 {
		return nil, invalid
	}
	status, err := strconv.Atoi(strings.TrimSpace(raw[index+len(separator):]))
	if err != nil || status < 200 || status > 599 {
		return nil, invalid
	}
	reader := bufio.NewReader(strings.NewReader(raw[:index]))
	for n := 0; n < 8; n++ {
		line, err := reader.ReadString('\n')
		fields := strings.Fields(line)
		if err != nil || len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
			return nil, invalid
		}
		code, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, invalid
		}
		headers, err := textproto.NewReader(reader).ReadMIMEHeader()
		if err != nil {
			return nil, invalid
		}
		if code >= 100 && code < 200 {
			continue
		}
		if code != status {
			return nil, invalid
		}
		selected := http.Header{}
		for _, key := range []string{"Content-Type", "X-Request-Id", "Request-Id", "Cf-Mitigated", "Cf-Ray"} {
			if value := headers.Get(key); value != "" {
				selected.Set(key, value)
			}
		}
		body, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			return nil, invalid
		}
		return &http.Response{StatusCode: status, Header: selected, Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Request: req}, nil
	}
	return nil, invalid
}
