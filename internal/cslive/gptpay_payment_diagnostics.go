// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

// 只提取状态和代码，Observer 再次白名单过滤；不发送 next_action 的完整内容。
func (t *gptpay_transaction) observePaymentDiagnostics(data protocol_Map) {
	t.providerCode, t.declineCode = t.emitPaymentDiagnostics(data)
}

// Intermediate replies are diagnostic evidence only. Recording them must not
// change the payment state or the error classification of the final result.
func (t *gptpay_transaction) emitPaymentDiagnostics(data protocol_Map) (providerCode, declineCode string) {
	nodes := []protocol_Map{data, protocol_AsMap(data["payment_intent"]), protocol_AsMap(protocol_AsMap(data["submission_attempt"])["payment_intent"]), protocol_AsMap(protocol_AsMap(data["error"])["payment_intent"]), protocol_AsMap(data["submission_attempt"])}
	sources := []string{"response", "payment_intent", "submission_payment_intent", "error_payment_intent", "submission_attempt"}
	classified := string(stripe_Classify(data))
	for i, node := range nodes {
		action := protocol_AsMap(node["next_action"])
		failure := protocol_AsMap(node["last_payment_error"])
		if len(node) == 0 {
			continue
		}
		fields := protocol_Map{"outcome": "observed", "diagnostic_source": sources[i], "payment_state": classified}
		for _, key := range []string{"status", "state", "payment_status"} {
			if value, ok := node[key]; ok {
				target := map[string]string{"status": "raw_status", "state": "raw_state", "payment_status": "raw_payment_status"}[key]
				fields[target] = value
			}
		}
		if typ := action["type"]; typ != nil {
			fields["next_action_type"] = typ
		}
		sdk := protocol_AsMap(action["use_stripe_sdk"])
		if typ := sdk["type"]; typ != nil {
			fields["sdk_action_type"] = typ
		}
		// 只报告已知字段是否存在，不读取验证码、跳转地址或指纹载荷。
		if len(sdk) > 0 {
			for _, key := range []string{"stripe_js", "three_d_secure_2_source", "server_transaction_id", "directory_server_name"} {
				value, exists := sdk[key]
				fields["sdk_has_"+key] = exists && value != nil
			}
		}
		if code := failure["code"]; code != nil {
			if classified == "not_charged" {
				providerCode = observability_SafePaymentCode(protocol_AsString(code), false)
				declineCode = observability_SafePaymentCode(protocol_AsString(failure["decline_code"]), true)
			}
			fields["provider_code"] = code
		}
		if code := failure["decline_code"]; code != nil {
			fields["decline_code"] = code
		}
		t.observer.Emit("payment_diagnostic", fields)
	}
	return
}
