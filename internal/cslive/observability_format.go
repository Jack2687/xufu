// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	strconv "strconv"
	strings "strings"
	time "time"
)

const observability_slowOperationMS = 5000

// FormatTimestamp 使用运行机器的时区；不使用协议的国家、时区或指纹偏移。
func observability_FormatTimestamp(at time.Time) string {
	return at.In(time.Local).Format("2006-01-02T15:04:05.000-07:00")
}

func observability_IsKeyEvent(e model_Event) bool {
	if protocol_AsString(e["warningCode"]) != "" {
		return true
	}
	event := protocol_AsString(e["event"])
	if protocol_StringSet("transaction", "phase", "price", "payment_confirm", "payment_error", "payment_diagnostic", "checkout_route", "radar_session", "radar_attachment", "account")[event] || event == "checkpoint" && e["outcome"] == "failed" {
		return true
	}
	if event != "http_request" && event != "sentinel" {
		return false
	}
	if e["outcome"] == "retry" || e["outcome"] == "failed" {
		return true
	}
	duration, ok := e["durationMs"].(float64)
	if !ok {
		value, _ := protocol_Integer(e["durationMs"])
		duration = float64(value)
	}
	return duration >= observability_slowOperationMS
}

func observability_FormatEvent(e model_Event) string {
	level := "DEBUG"
	if observability_IsKeyEvent(e) {
		level = "INFO"
		if e["outcome"] == "failed" || protocol_AsString(e["warningCode"]) != "" {
			level = "WARNING"
		}
	}
	parts := []string{}
	for _, k := range strings.Fields("event traceId operation phase service method path checkoutPrefix paymentRoute flow planType renewal activeUntil executor browserProfile attempt status outcome paymentState intentStatus diagnosticSource rawStatus rawState rawPaymentStatus nextActionType sdkActionType sdkHasStripeJS sdkHasThreeDS2Source sdkHasServerTransactionID sdkHasDirectoryServerName errorCode warningCode providerCode declineCode requestId responseContentType responseErrorCode responseErrorType responseMessage responseDetail responseTitle responseSummary responseTruncated cfRay responseRequestId paymentElapsedMs durationMs elapsedMs remainingDeadlineMs") {
		v, ok := e[k]
		if ok && v != nil && protocol_AsString(v) != "" {
			value := protocol_AsString(v)
			switch k {
			case "sdkHasStripeJS", "sdkHasThreeDS2Source", "sdkHasServerTransactionID", "sdkHasDirectoryServerName":
				b, ok := v.(bool)
				if !ok {
					continue
				}
				value = strconv.FormatBool(b)
			case "diagnosticSource":
				value = observability_SafeDiagnosticSource(value)
			case "rawStatus", "rawState", "rawPaymentStatus":
				value = observability_SafePaymentStatus(value)
			case "nextActionType", "sdkActionType":
				value = observability_SafeActionType(value)
			case "warningCode":
				value = observability_SafeErrorCode(v)
			case "paymentElapsedMs":
				elapsed, ok := observability_paymentElapsedMilliseconds(v)
				if !ok {
					continue
				}
				value = protocol_AsString(elapsed)
			case "responseMessage", "responseDetail", "responseTitle", "responseSummary":
				value = strconv.Quote(observability_SafeResponseText(value, nil))
			case "responseErrorCode", "responseErrorType":
				value = observability_safeResponseCode(value, nil)
			case "responseContentType":
				value = observability_safeContentType(value)
			case "responseTruncated":
				truncated, ok := v.(bool)
				if !ok {
					continue
				}
				value = strconv.FormatBool(truncated)
			case "cfRay":
				if !observability_diagnosticRay.MatchString(value) {
					continue
				}
			case "responseRequestId":
				if !observability_diagnosticRequestID.MatchString(value) {
					continue
				}
			case "checkoutPrefix":
				if !protocol_StringSet("oaics_", "cs_live_")[value] {
					continue
				}
			case "paymentRoute":
				if !protocol_StringSet("chatgpt", "stripe")[value] {
					continue
				}
			case "browserProfile":
				if !protocol_StringSet("chrome131-windows-v1", "chrome131-macos-v1")[value] {
					continue
				}
			}
			if value != "" {
				parts = append(parts, k+"="+value)
			}
		}
	}
	// 只转换显示，保留 EventSink 收到的 UTC 时间戳和原始事件对象。
	stamp := protocol_AsString(e["timestamp"])
	if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		stamp = observability_FormatTimestamp(parsed)
	}
	return fmt.Sprintf("%s %-7s protocol %s", stamp, level, strings.Join(parts, " "))
}
