// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

// 仅识别已经确认失败的税务错误，不让未知上游文本进入日志或覆盖已扣款结果。
func gptpay_customSubmissionError(data protocol_Map) error {
	if stripe_Classify(data) != model_PaymentDeclined {
		return nil
	}
	submission := protocol_AsMap(data["submission_attempt"])
	failure := protocol_AsMap(submission["error"])
	if failure["code"] == "billing_automatic_tax_unrecognized_customer_location" {
		return protocol_NewError("billing_automatic_tax_unrecognized_customer_location")
	}
	return nil
}
