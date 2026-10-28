// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	base64 "encoding/base64"
	errors "errors"
	strings "strings"
	time "time"
)

// Sentinel 只生成绑定本次 flow 的证明，不跨交易缓存令牌。
type challenge_Sentinel struct {
	Profile  protocol_BrowserProfile
	http     protocol_HTTPTransport
	cp       *model_Checkpoint
	clock    fingerprint_Clock
	observer *observability_Observer
}

func challenge_NewSentinel(http protocol_HTTPTransport, cp *model_Checkpoint, clock fingerprint_Clock, observer *observability_Observer) *challenge_Sentinel {
	return &challenge_Sentinel{Profile: protocol_DefaultBrowserProfile(), http: http, cp: cp, clock: clock, observer: observer}
}

func (s *challenge_Sentinel) Token(flow string) (token [2]string, err error) {
	started := time.Now()
	defer func() {
		outcome := "succeeded"
		if err != nil {
			outcome = "failed"
		}
		s.observer.Emit("sentinel", protocol_Map{"flow": flow, "executor": "local_vm", "outcome": outcome, "error_code": protocol_FlowErrorCode(err), "duration_ms": time.Since(started).Seconds() * 1000})
	}()
	token, err = s.local(flow)
	var ve *sentinel_Error
	if errors.As(err, &ve) {
		err = protocol_NewError(ve.Code)
	}
	return
}

func (s *challenge_Sentinel) local(flow string) ([2]string, error) {
	cp := s.cp
	profile := s.Profile
	deadline := s.http.Deadline()
	check := func() error {
		if !time.Now().Before(deadline) {
			return protocol_NewError("deadline_exceeded")
		}
		return nil
	}
	if e := check(); e != nil {
		return [2]string{}, e
	}
	generator := sentinel_NewGenerator(cp.DeviceID, profile, "en-US", cp.Timezone, s.clock)
	requirements, e := generator.RequirementsToken()
	if e != nil {
		return [2]string{}, e
	}
	headers := profile.Headers()
	headers.Update(protocol_O("Authorization", "Bearer "+cp.AccessToken, "Cookie", cp.Cookies, "oai-device-id", cp.DeviceID, "Accept", "*/*", "Content-Type", "text/plain;charset=UTF-8", "Origin", chatgpt_PlatformBase, "Referer", chatgpt_PlatformBase+"/backend-api/sentinel/frame.html?sv="+sentinel_FrameVersion))
	body, e := protocol_CompactJSON(protocol_O("p", requirements, "id", cp.DeviceID, "flow", flow))
	if e != nil {
		return [2]string{}, e
	}
	response, e := s.http.Request("POST", chatgpt_PlatformBase+"/backend-api/sentinel/req", protocol_RequestOptions{Headers: headers, Text: &body})
	if e != nil {
		return [2]string{}, e
	}
	data, e := protocol_JSONResponse(response, false)
	if e != nil {
		return [2]string{}, e
	}
	if e = check(); e != nil {
		return [2]string{}, e
	}
	challenge := protocol_FirstTruthy(data["token"], data["c"])
	var pow protocol_Map
	for _, k := range []string{"proofofwork", "proofOfWork", "proof_of_work"} {
		if m, ok := data[k].(protocol_Map); ok {
			pow = m
			break
		}
	}
	turnstile := protocol_AsMap(protocol_FirstTruthy(data["turnstile"], data["Turnstile"]))
	proof := requirements
	t := ""
	if !protocol_Truthy(challenge) {
		return [2]string{}, protocol_NewError("sentinel_challenge_missing")
	}
	if pow["required"] == true {
		if !protocol_Truthy(pow["seed"]) {
			return [2]string{}, protocol_NewError("sentinel_pow_missing")
		}
		proof, e = generator.PowToken(protocol_AsString(pow["seed"]), protocol_AsString(protocol_FirstTruthy(pow["difficulty"], "0")), deadline)
		if e != nil {
			return [2]string{}, e
		}
	}
	required := turnstile["required"] == true
	if required {
		if !protocol_Truthy(turnstile["dx"]) {
			return [2]string{}, protocol_NewError("sentinel_dx_missing")
		}
		t, e = sentinel_SolveTurnstileDX(protocol_AsString(turnstile["dx"]), requirements, sentinel_Options{Deadline: deadline, Profile: profile, Cookies: cp.Cookies, Clock: s.clock, Bootstrap: protocol_Map{"session": protocol_Map{"account": protocol_Map{"id": cp.AccountID}}, "account": protocol_Map{"id": cp.AccountID}}})
		if e != nil {
			return [2]string{}, e
		}
	}
	payload := protocol_O("p", proof, "t", t, "c", challenge, "id", cp.DeviceID, "flow", flow)
	if e = s.Validate(protocol_AsMap(payload), flow, required); e != nil {
		return [2]string{}, e
	}
	encoded, e := protocol_CompactJSON(payload)
	if e != nil {
		return [2]string{}, e
	}
	timing := chatgpt_Telemetry()
	if e = check(); e != nil {
		return [2]string{}, e
	}
	return [2]string{encoded, timing}, nil
}

func (s *challenge_Sentinel) Validate(payload protocol_Map, flow string, requiredT bool) error {
	for _, k := range []string{"p", "c", "id", "flow"} {
		if !protocol_Truthy(payload[k]) {
			return protocol_NewError("sentinel_incomplete")
		}
	}
	if payload["flow"] != flow || payload["id"] != s.cp.DeviceID {
		return protocol_NewError("sentinel_context_mismatch")
	}
	t := protocol_AsString(payload["t"])
	if requiredT && t == "" {
		return protocol_NewError("sentinel_turnstile_missing")
	}
	decoded, _ := base64.StdEncoding.DecodeString(t + strings.Repeat("=", (4-len(t)%4)%4))
	if strings.HasPrefix(string(decoded), "0: ") || strings.Contains(string(decoded), "SyntaxError") {
		return protocol_NewError("sentinel_turnstile_error")
	}
	return nil
}
