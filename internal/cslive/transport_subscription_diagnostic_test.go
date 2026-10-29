package cslive

import (
	"net/http"
	"testing"
	"time"
)

func TestSubscription404DiagnosticRequiresExactBusinessResponse(t *testing.T) {
	for _, tc := range []struct{ url, body, want string }{
		{"https://chatgpt.com/backend-api/subscriptions?account_id=private", `{"detail":"No subscription found for account"}`, "subscription_not_found"},
		{"https://chatgpt.com/backend-api/subscriptions", `{"detail":"Not Found"}`, "http_404"},
		{"https://chatgpt.com/backend-api/accounts/check/v4", `{"detail":"No subscription found for account"}`, "http_404"},
		{"https://other.example/backend-api/subscriptions", `{"detail":"No subscription found for account"}`, "http_404"},
	} {
		var event model_Event
		transport := &transport_Transport{expires: time.Now().Add(time.Minute), observer: observability_NewObserver("test", func(e model_Event) { event = e }, "", "")}
		response := &protocol_HTTPResponse{StatusCode: 404, Content: []byte(tc.body), Headers: http.Header{"Content-Type": {"application/json"}}}
		transport.observe("GET", tc.url, 1, time.Now(), time.Now(), response, nil, false, protocol_RequestOptions{}, nil)
		status, ok := protocol_Integer(event["status"])
		if event["errorCode"] != tc.want || !ok || status != 404 {
			t.Fatalf("got %v want %s", event, tc.want)
		}
	}
}
