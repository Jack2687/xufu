// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	math "math"
	big "math/big"
	strconv "strconv"
	strings "strings"
	utf16 "unicode/utf16"
	utf8 "unicode/utf8"
)

func sentinel_quoteJSON(s string, ascii bool) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("invalid UTF-8 string")
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if r < 32 || (ascii && r > 127) {
				if r <= 0xffff {
					fmt.Fprintf(&b, "\\u%04x", r)
				} else {
					hi, lo := utf16.EncodeRune(r)
					fmt.Fprintf(&b, "\\u%04x\\u%04x", hi, lo)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

func sentinel_dumpJSON(value any, ascii, allowNaN bool) (string, error) {
	return sentinel_dumpDepth(value, ascii, allowNaN, map[any]bool{}, 0)
}
func sentinel_dumpDepth(value any, ascii, allowNaN bool, seen map[any]bool, depth int) (string, error) {
	if depth > 64 {
		return "", fmt.Errorf("maximum recursion depth exceeded while encoding a JSON object")
	}
	switch v := value.(type) {
	case nil:
		return "null", nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case string:
		return sentinel_quoteJSON(v, ascii)
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case *big.Int:
		return v.String(), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			if !allowNaN {
				return "", fmt.Errorf("Out of range float values are not JSON compliant")
			}
			if math.IsNaN(v) {
				return "NaN", nil
			}
			if math.IsInf(v, 1) {
				return "Infinity", nil
			}
			return "-Infinity", nil
		}
		return sentinel_pythonFloat(v), nil
	case *sentinel_array:
		if seen[v] {
			return "", fmt.Errorf("Circular reference detected")
		}
		seen[v] = true
		defer delete(seen, v)
		values := make([]string, len(v.values))
		for i, child := range v.values {
			s, err := sentinel_dumpDepth(child, ascii, allowNaN, seen, depth+1)
			if err != nil {
				return "", err
			}
			values[i] = s
		}
		return "[" + strings.Join(values, ",") + "]", nil
	case *sentinel_object:
		if seen[v] {
			return "", fmt.Errorf("Circular reference detected")
		}
		seen[v] = true
		defer delete(seen, v)
		values := make([]string, len(v.entries))
		for i, item := range v.entries {
			var key string
			if s, ok := item.key.(string); ok {
				key = s
			} else {
				var err error
				key, err = sentinel_dumpDepth(item.key, ascii, allowNaN, seen, depth+1)
				if err != nil {
					return "", err
				}
			}
			key, err := sentinel_quoteJSON(key, ascii)
			if err != nil {
				return "", err
			}
			s, err := sentinel_dumpDepth(item.value, ascii, allowNaN, seen, depth+1)
			if err != nil {
				return "", err
			}
			values[i] = key + ":" + s
		}
		return "{" + strings.Join(values, ",") + "}", nil
	}
	return "", fmt.Errorf("Object of type %s is not JSON serializable", sentinel_typeName(value))
}

// 小型 JSON 读取器保留对象插入顺序、任意精度整数与 Python 常量解析。
type sentinel_jsonReader struct {
	s     string
	i     int
	depth int
}

func sentinel_parseJSON(s string) (any, error) {
	p := sentinel_jsonReader{s: s}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(s) {
		return nil, p.fail("Extra data")
	}
	return v, nil
}
func (p *sentinel_jsonReader) space() {
	for p.i < len(p.s) && strings.ContainsRune(" \r\n\t", rune(p.s[p.i])) {
		p.i++
	}
}
func (p *sentinel_jsonReader) fail(message string) error {
	line := 1 + strings.Count(p.s[:p.i], "\n")
	col := p.i - strings.LastIndex(p.s[:p.i], "\n")
	return fmt.Errorf("%s: line %d column %d (char %d)", message, line, col, p.i)
}
func (p *sentinel_jsonReader) value() (any, error) {
	p.space()
	if p.depth > 65 {
		return nil, sentinel_resourceError("turnstile_data_depth_limit")
	}
	if p.i >= len(p.s) {
		return nil, p.fail("Expecting value")
	}
	for _, c := range []struct {
		text  string
		value any
	}{{"null", nil}, {"true", true}, {"false", false}, {"NaN", math.NaN()}, {"Infinity", math.Inf(1)}, {"-Infinity", math.Inf(-1)}} {
		if strings.HasPrefix(p.s[p.i:], c.text) {
			p.i += len(c.text)
			return c.value, nil
		}
	}
	switch p.s[p.i] {
	case '"':
		return p.stringValue()
	case '[':
		p.i++
		p.depth++
		defer func() { p.depth-- }()
		a := sentinel_arr()
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return a, nil
		}
		for {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			a.values = append(a.values, v)
			p.space()
			if p.i >= len(p.s) {
				return nil, p.fail("Expecting ',' delimiter")
			}
			c := p.s[p.i]
			p.i++
			if c == ']' {
				return a, nil
			}
			if c != ',' {
				return nil, p.fail("Expecting ',' delimiter")
			}
		}
	case '{':
		p.i++
		p.depth++
		defer func() { p.depth-- }()
		o := sentinel_obj()
		p.space()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return o, nil
		}
		for {
			p.space()
			if p.i >= len(p.s) || p.s[p.i] != '"' {
				return nil, p.fail("Expecting property name enclosed in double quotes")
			}
			k, err := p.stringValue()
			if err != nil {
				return nil, err
			}
			p.space()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return nil, p.fail("Expecting ':' delimiter")
			}
			p.i++
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			_ = o.set(k, v)
			p.space()
			if p.i >= len(p.s) {
				return nil, p.fail("Expecting ',' delimiter")
			}
			c := p.s[p.i]
			p.i++
			if c == '}' {
				return o, nil
			}
			if c != ',' {
				return nil, p.fail("Expecting ',' delimiter")
			}
		}
	default:
		start := p.i
		if p.s[p.i] == '-' {
			p.i++
		}
		if p.i >= len(p.s) || p.s[p.i] < '0' || p.s[p.i] > '9' {
			p.i = start
			return nil, p.fail("Expecting value")
		}
		if p.s[p.i] == '0' {
			p.i++
		} else {
			for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
				p.i++
			}
		}
		isFloat := false
		if p.i+1 < len(p.s) && p.s[p.i] == '.' && p.s[p.i+1] >= '0' && p.s[p.i+1] <= '9' {
			isFloat = true
			p.i++
			for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
				p.i++
			}
		}
		if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
			j := p.i + 1
			if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
				j++
			}
			if j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
				isFloat = true
				p.i = j + 1
				for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
					p.i++
				}
			}
		}
		text := p.s[start:p.i]
		if isFloat {
			f, err := strconv.ParseFloat(text, 64)
			if err != nil && !math.IsInf(f, 0) {
				return nil, err
			}
			return f, nil
		}
		if len(strings.TrimPrefix(text, "-")) > 4300 {
			return nil, fmt.Errorf("Exceeds the limit (4300 digits) for integer string conversion")
		}
		n, ok := new(big.Int).SetString(text, 10)
		if !ok {
			return nil, p.fail("Expecting value")
		}
		return sentinel_compactInt(n), nil
	}
}
func (p *sentinel_jsonReader) stringValue() (string, error) {
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		if c == '"' {
			return b.String(), nil
		}
		if c < 32 {
			return "", p.fail("Invalid control character at")
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if p.i >= len(p.s) {
			return "", p.fail("Unterminated string starting at")
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			if p.i+4 > len(p.s) {
				return "", p.fail("Invalid \\uXXXX escape")
			}
			n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 16)
			if err != nil {
				return "", p.fail("Invalid \\uXXXX escape")
			}
			p.i += 4
			r := rune(n)
			if utf16.IsSurrogate(r) {
				if r >= 0xd800 && r <= 0xdbff && p.i+6 <= len(p.s) && p.s[p.i:p.i+2] == "\\u" {
					lo, err := strconv.ParseUint(p.s[p.i+2:p.i+6], 16, 16)
					if err == nil && lo >= 0xdc00 && lo <= 0xdfff {
						p.i += 6
						r = utf16.DecodeRune(r, rune(lo))
					}
				}
				if utf16.IsSurrogate(r) {
					return "", fmt.Errorf("utf-8 codec cannot encode surrogate")
				}
			}
			b.WriteRune(r)
		default:
			return "", p.fail("Invalid \\escape")
		}
	}
	return "", p.fail("Unterminated string starting at")
}
