package platform

import "strings"

var actionDiagnosticEnums = map[string]string{
	"sdk_stage":    "browser_start page_loading sdk_loaded context_ready preflight preflight_returned preflight_error sdk_action sdk_returned sdk_exception verifying notifying",
	"sdk_resource": "sdk_script sdk_frame processor_api verification_resource",
	"sdk_event":    "progress diagnostic",
	"sdk_issue":    "csp_blocked resource_failed resource_http_error guard_timeout page_error",
}

func safeActionDiagnostic(key string, value any) string {
	s, ok := value.(string)
	if ok && s != "" && strings.Contains(" "+actionDiagnosticEnums[key]+" ", " "+s+" ") {
		return s
	}
	return ""
}
