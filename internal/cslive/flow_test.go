package cslive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

type scriptedTransport struct {
	t       *testing.T
	request func(string, string, protocol_RequestOptions) (*protocol_HTTPResponse, error)
	calls   []string
	pauses  []float64
}

func (s *scriptedTransport) Request(method, target string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
	u, err := url.Parse(target)
	if err != nil {
		s.t.Fatal(err)
	}
	s.calls = append(s.calls, method+" "+u.Path)
	return s.request(method, u.Path, opts)
}
func (s *scriptedTransport) Pause(n float64) error { s.pauses = append(s.pauses, n); return nil }
func (s *scriptedTransport) Close()                {}
func (s *scriptedTransport) Deadline() time.Time   { return time.Now().Add(time.Minute) }
func response(status int, v any) *protocol_HTTPResponse {
	b, _ := json.Marshal(v)
	return &protocol_HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Content: b, Cookies: map[string]string{}}
}

type fixedProof struct{}

func (fixedProof) Token(string) ([2]string, error) {
	return [2]string{`{"p":"proof","t":"turnstile"}`, "telemetry"}, nil
}

func flowFixture(t *testing.T, cancel bool) (*gptpay_transaction, *scriptedTransport, *[]model_Checkpoint) {
	t.Helper()
	transport := &scriptedTransport{t: t}
	saved := []model_Checkpoint{}
	client := gptpay_NewPaymentClient(model_DefaultConfig(), gptpay_ClientOptions{Checkpoint: func(cp model_Checkpoint) error { saved = append(saved, cp); return nil }})
	client.transportFactory = func(context.Context, string, float64, time.Time) (protocol_HTTPTransport, error) {
		return transport, nil
	}
	client.sentinelFactory = func(protocol_HTTPTransport, *model_Checkpoint, fingerprint_Clock) gptpay_tokenSource {
		return fixedProof{}
	}
	cp, err := client.newCheckpoint(model_Session{AccessToken: "old-at", SessionToken: "session", AccountID: "account", Email: "fixture@example.com"}, "PH", "pro20x", cancel, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	clock, _ := fingerprint_NewClock(0)
	flow, err := gptpay_newTransaction(context.Background(), client, cp, "pay", clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(flow.close)
	return flow, transport, &saved
}

func TestPrepareUsesReferenceOrderAndRefreshedAuthorization(t *testing.T) {
	flow, transport, saved := flowFixture(t, true)
	transport.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
		switch path {
		case "/":
			r := response(200, "")
			r.Content = []byte(`<html data-build="build" data-seq="42"></html>`)
			r.Headers.Add("Set-Cookie", "oai-did=11111111-1111-4111-8111-111111111111; Path=/; Secure")
			return r, nil
		case "/api/auth/session":
			if opts.Headers.Get("Authorization") != nil {
				t.Fatal("refresh sent bearer token")
			}
			return response(200, protocol_Map{"accessToken": "new-at", "account": protocol_Map{"id": "account"}}), nil
		case "/backend-api/accounts/check/v4-2023-04-27":
			if opts.Headers.Get("Authorization") != "Bearer new-at" {
				t.Fatal("account read did not use refreshed token")
			}
			return response(200, protocol_Map{"accounts": protocol_Map{"account": protocol_Map{"account": protocol_Map{"id": "account", "account_type": "personal", "plan_type": "free"}, "entitlement": protocol_Map{"has_active_subscription": false}}}}), nil
		case "/backend-api/subscriptions":
			return response(404, protocol_Map{"detail": "No subscription found for account"}), nil
		case "/backend-api/payments/checkout":
			if len(*saved) < 2 || (*saved)[len(*saved)-1].Phase != "account_checked" {
				t.Fatal("checkout preceded durable account checkpoint")
			}
			return response(200, protocol_Map{"checkout_session_id": "cs_live_Fixture", "checkout_ui_mode": "custom", "publishable_key": "pk_test_Fixture"}), nil
		default:
			t.Fatalf("unexpected request %s %s", method, path)
			return nil, nil
		}
	}
	_, err := flow.prepareCheckout("hosted", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /", "GET /api/auth/session", "GET /backend-api/accounts/check/v4-2023-04-27", "GET /backend-api/subscriptions", "POST /backend-api/payments/checkout"}
	if !reflect.DeepEqual(transport.calls, want) {
		t.Fatalf("sequence %v", transport.calls)
	}
}

func TestPostPaymentSequenceAndBoundedActivation(t *testing.T) {
	for _, mode := range []string{"cancel", "keep", "activation401", "pending", "refreshRejected"} {
		t.Run(mode, func(t *testing.T) {
			flow, tr, saved := flowFixture(t, mode != "keep")
			flow.cp.Payment = "charged"
			reads, refreshes, cancels := 0, 0, 0
			tr.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
				switch path {
				case "/api/auth/session":
					refreshes++
					if mode == "refreshRejected" {
						return response(403, protocol_Map{}), nil
					}
					return response(200, protocol_Map{"accessToken": "renewed-at", "account": protocol_Map{"id": "account"}}), nil
				case "/backend-api/subscriptions":
					reads++
					if mode == "activation401" && reads == 1 {
						return response(401, protocol_Map{}), nil
					}
					if mode == "pending" {
						return response(404, protocol_Map{"detail": "No subscription found for account"}), nil
					}
					return response(200, protocol_Map{"plan_type": "premium", "will_renew": cancels == 0, "active_until": "2099-01-01T00:00:00Z"}), nil
				case "/backend-api/subscriptions/cancel":
					if (*saved)[len(*saved)-1].Phase != "cancellation_requested" {
						t.Fatal("cancel before durable checkpoint")
					}
					cancels++
					return response(200, protocol_Map{}), nil
				default:
					t.Fatalf("unexpected %s %s", method, path)
					return nil, nil
				}
			}
			err := flow.finishPayment()
			switch mode {
			case "pending":
				if ErrorCode(err) != "activation_pending" || reads != 5 || len(tr.pauses) != 4 || refreshes != 0 || cancels != 0 {
					t.Fatalf("unbounded activation: %d %d %v", reads, refreshes, err)
				}
			case "refreshRejected":
				if ErrorCode(err) != "http_403" || cancels != 0 {
					t.Fatal("cancel after rejected refresh", err)
				}
			default:
				if err != nil || flow.cp.Activation != "active" {
					t.Fatal(err)
				}
				wantRefresh, wantCancel := 1, 1
				if mode == "keep" {
					wantRefresh, wantCancel = 0, 0
				}
				if mode == "activation401" {
					wantRefresh = 2
				}
				if refreshes != wantRefresh || cancels != wantCancel {
					t.Fatalf("refresh=%d cancel=%d", refreshes, cancels)
				}
			}
			if flow.cp.Payment != "charged" {
				t.Fatal("post-payment error lost charged state")
			}
		})
	}
}

func TestRequestGuardPreventsNetworkAndRetry(t *testing.T) {
	tr, err := transport_NewTransport(context.Background(), "http://127.0.0.1:1", 1, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	calls := 0
	guardErr := errors.New("fixed exit changed")
	tr.beforeRequest = func(context.Context, string, string) error { calls++; return guardErr }
	_, err = tr.Request("GET", "https://chatgpt.com/", protocol_RequestOptions{})
	if !errors.Is(err, guardErr) || calls != 1 {
		t.Fatal("guard failure retried or lost", err, calls)
	}
}
