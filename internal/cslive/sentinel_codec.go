// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	base64 "encoding/base64"
	fmt "fmt"
	utf8 "unicode/utf8"
)

func sentinel_latin1Bytes(s string) ([]byte, error) {
	b := make([]byte, 0, utf8.RuneCountInString(s))
	for _, r := range s {
		if r > 255 {
			return nil, fmt.Errorf("latin-1 codec cannot encode character")
		}
		b = append(b, byte(r))
	}
	return b, nil
}
func sentinel_latin1String(data []byte) string {
	r := make([]rune, len(data))
	for i, b := range data {
		r[i] = rune(b)
	}
	return string(r)
}
func sentinel_b64EncodeLatin1(s string) (string, error) {
	b, err := sentinel_latin1Bytes(s)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
func sentinel_b64DecodeLatin1(s string) (string, error) {
	// Python b64decode(validate=False) 忽略非字母表字符及多余 padding。
	var out []byte
	quartet := make([]byte, 0, 4)
	pads := 0
	for _, r := range s {
		if r > 127 {
			return "", fmt.Errorf("ascii codec cannot encode character")
		}
		if r == '=' {
			if len(quartet) == 3 {
				quartet = append(quartet, '=')
				decoded, err := base64.StdEncoding.DecodeString(string(quartet))
				if err != nil {
					return "", err
				}
				return sentinel_latin1String(append(out, decoded...)), nil
			}
			if len(quartet) == 2 {
				pads++
				if pads == 2 {
					quartet = append(quartet, '=', '=')
					decoded, err := base64.StdEncoding.DecodeString(string(quartet))
					if err != nil {
						return "", err
					}
					return sentinel_latin1String(append(out, decoded...)), nil
				}
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/') {
			continue
		}
		pads = 0
		quartet = append(quartet, byte(r))
		if len(quartet) == 4 {
			decoded, err := base64.StdEncoding.DecodeString(string(quartet))
			if err != nil {
				return "", err
			}
			out = append(out, decoded...)
			quartet = quartet[:0]
		}
	}
	if len(quartet) != 0 {
		return "", fmt.Errorf("Incorrect padding")
	}
	return sentinel_latin1String(out), nil
}
func sentinel_xorLatin1(data, key string) string {
	if key == "" {
		return data
	}
	r, k := []rune(data), []rune(key)
	for i := range r {
		r[i] ^= k[i%len(k)]
	}
	return string(r)
}
