// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	base64 "encoding/base64"
	url "net/url"
	regexp "regexp"
	strings "strings"
)

// EncodeStripeBlob 保留 Python 的 ASCII JSON、1..3 字节填充及后续逐字节变换。
func stripe_EncodeStripeBlob(value any) (string, error) {
	plain, err := protocol_CompactJSON(value)
	if err != nil {
		return "", err
	}
	padded := []byte(plain + strings.Repeat(" ", 3-len(plain)%3))
	for i := range padded {
		padded[i] ^= 5
	}
	encoded := []byte(url.QueryEscape(base64.StdEncoding.EncodeToString(padded)))
	for i, c := range encoded {
		encoded[i] = (c-32+11)%95 + 32
	}
	return string(encoded), nil
}

func stripe_ComputeIDChecksum(id string) (string, error) {
	value := strings.TrimSpace(id)
	if value == "" {
		return "", &protocol_ProtocolError{Code: "checkout_id_invalid"}
	}
	return stripe_EncodeStripeBlob(protocol_O("id", value))
}

func stripe_ComputeJSChecksum(paymentMethodID string) (string, error) {
	pm := strings.TrimSpace(paymentMethodID)
	if !strings.HasPrefix(pm, "pm_") {
		return "", &protocol_ProtocolError{Code: "invalid_payment_method"}
	}
	return stripe_ComputeIDChecksum(pm)
}

func stripe_ComputeRVTimestamp(rvTS, rv, sv string) (string, error) {
	if rvTS == "" || rv == "" || sv == "" {
		return "", &protocol_ProtocolError{Code: "stripe_build_missing"}
	}
	return stripe_EncodeStripeBlob(protocol_O("rvTs", rvTS, "rv", rv, "sv", sv))
}

func stripe_ParseStripeBuildConstants(source string) (map[string]string, error) {
	find := func(pattern string) string {
		match := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if len(match) > 1 {
			return match[1]
		}
		return ""
	}
	pair := regexp.MustCompile(`STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{10})\"\s*,\s*o\s*=\s*/\*!?\s*STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{40})\"`).FindStringSubmatch(source)
	version, rv := "", ""
	if len(pair) > 2 {
		version, rv = pair[1], pair[2]
	} else {
		version = find(`STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{10})\"`)
		rv = find(`STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{40})\"`)
	}
	if version == "" || rv == "" {
		return nil, &protocol_ProtocolError{Code: "stripe_build_missing"}
	}
	rvTS := find(`rvTs["']?\s*:\s*["']([^"']+)["']`)
	if rvTS == "" {
		rvTS = find(`"(\d{4}-\d{2}-\d{2} 00:00:00 -0000)"`)
	}
	if rvTS == "" {
		rvTS = find(`r="(\d{4}-\d{2}-\d{2} 00:00:00 -0000)"`)
	}
	sv := find(`sv:\s*/\*!?\s*STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{64})\"`)
	if sv == "" {
		sv = find(regexp.QuoteMeta(rv) + `"\s*,\s*a\s*=\s*/\*!?\s*STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{64})\"`)
	}
	if sv == "" {
		sv = find(`STRIPE_JS_BUILD_SALT[^\"]*\"([a-f0-9]{64})\"`)
	}
	if rvTS == "" || sv == "" {
		return nil, &protocol_ProtocolError{Code: "stripe_build_missing"}
	}
	return map[string]string{"version": version, "rv": rv, "sv": sv, "rv_ts": rvTS}, nil
}
