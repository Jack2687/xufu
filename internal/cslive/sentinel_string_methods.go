// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	cases "golang.org/x/text/cases"
	language "golang.org/x/text/language"
	strings "strings"
	unicode "unicode"
	utf8 "unicode/utf8"
)

func sentinel_upper(s string) string { return cases.Upper(language.Und).String(s) }

func sentinel_checkStringAllocation(value, name string, args []any, limit int) error {
	if name == "replace" && len(args) >= 2 {
		old, ok := args[0].(string)
		if !ok {
			return nil
		}
		replacement, ok := args[1].(string)
		if !ok {
			return nil
		}
		count := strings.Count(value, old)
		if len(args) > 2 {
			n, err := sentinel_integerIndex(args[2])
			if err != nil {
				return nil
			}
			if n >= 0 {
				count = min(count, n)
			}
		}
		growth := len(replacement) - len(old)
		if growth > 0 && count > (limit-len(value))/growth {
			return sentinel_resourceError("turnstile_value_limit")
		}
	}
	if name == "join" && len(args) == 1 {
		list, err := sentinel_listCopy(args[0])
		if err != nil {
			return nil
		}
		total := 0
		for i, item := range list.values {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			total += len(s)
			if i > 0 {
				total += len(value)
			}
			if total > limit {
				return sentinel_resourceError("turnstile_value_limit")
			}
		}
	}
	return nil
}

func sentinel_stringArgument(value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("must be str, not %s", sentinel_typeName(value))
	}
	return s, nil
}

// 有 maxsplit 时保留剩余片段中的原始空白，不能先 Fields 再 Join。
func sentinel_splitWhitespace(value string, count int, reverse bool) []string {
	r := []rune(value)
	if reverse {
		var parts []string
		end := len(r)
		for end > 0 && unicode.IsSpace(r[end-1]) {
			end--
		}
		for end > 0 {
			if count == 0 {
				parts = append(parts, string(r[:end]))
				break
			}
			start := end
			for start > 0 && !unicode.IsSpace(r[start-1]) {
				start--
			}
			parts = append(parts, string(r[start:end]))
			count--
			end = start
			for end > 0 && unicode.IsSpace(r[end-1]) {
				end--
			}
		}
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		return parts
	}
	var parts []string
	start := 0
	for start < len(r) && unicode.IsSpace(r[start]) {
		start++
	}
	for start < len(r) {
		if count == 0 {
			parts = append(parts, string(r[start:]))
			break
		}
		end := start
		for end < len(r) && !unicode.IsSpace(r[end]) {
			end++
		}
		parts = append(parts, string(r[start:end]))
		count--
		start = end
		for start < len(r) && unicode.IsSpace(r[start]) {
			start++
		}
	}
	return parts
}
func sentinel_stringMethod(value, name string) *sentinel_callable {
	switch name {
	case "upper":
		return sentinel_native(0, 0, func([]any) (any, error) { return sentinel_upper(value), nil })
	case "lower":
		return sentinel_native(0, 0, func([]any) (any, error) { return cases.Lower(language.Und).String(value), nil })
	case "casefold":
		return sentinel_native(0, 0, func([]any) (any, error) { return cases.Fold().String(value), nil })
	case "capitalize":
		return sentinel_native(0, 0, func([]any) (any, error) {
			r := []rune(strings.ToLower(value))
			if len(r) > 0 {
				r[0] = unicode.ToUpper(r[0])
			}
			return string(r), nil
		})
	case "strip", "lstrip", "rstrip":
		return sentinel_native(0, 1, func(a []any) (any, error) {
			cut := unicode.IsSpace
			if len(a) > 0 && a[0] != nil {
				s, err := sentinel_stringArgument(a[0])
				if err != nil {
					return nil, err
				}
				cut = func(r rune) bool { return strings.ContainsRune(s, r) }
			}
			switch name {
			case "lstrip":
				return strings.TrimLeftFunc(value, cut), nil
			case "rstrip":
				return strings.TrimRightFunc(value, cut), nil
			}
			return strings.TrimFunc(value, cut), nil
		})
	case "replace":
		return sentinel_native(2, 3, func(a []any) (any, error) {
			old, err := sentinel_stringArgument(a[0])
			if err != nil {
				return nil, err
			}
			replacement, err := sentinel_stringArgument(a[1])
			if err != nil {
				return nil, err
			}
			count := -1
			if len(a) == 3 {
				count, err = sentinel_integerIndex(a[2])
				if err != nil {
					return nil, err
				}
			}
			return strings.Replace(value, old, replacement, count), nil
		})
	case "split", "rsplit":
		return sentinel_native(0, 2, func(a []any) (any, error) {
			count := -1
			if len(a) > 1 {
				var err error
				count, err = sentinel_integerIndex(a[1])
				if err != nil {
					return nil, err
				}
			}
			var values []string
			if len(a) == 0 || a[0] == nil {
				values = sentinel_splitWhitespace(value, count, name == "rsplit")
			} else {
				separator, err := sentinel_stringArgument(a[0])
				if err != nil {
					return nil, err
				}
				if separator == "" {
					return nil, fmt.Errorf("empty separator")
				}
				if name == "rsplit" && count >= 0 {
					all := strings.Split(value, separator)
					if len(all) > count+1 {
						values = append([]string{strings.Join(all[:len(all)-count], separator)}, all[len(all)-count:]...)
					} else {
						values = all
					}
				} else {
					n := count + 1
					if count < 0 {
						n = -1
					}
					values = strings.SplitN(value, separator, n)
				}
			}
			result := sentinel_arr()
			for _, s := range values {
				result.values = append(result.values, s)
			}
			return result, nil
		})
	case "join":
		return sentinel_native(1, 1, func(a []any) (any, error) {
			list, err := sentinel_listCopy(a[0])
			if err != nil {
				return nil, err
			}
			parts := make([]string, len(list.values))
			for i, v := range list.values {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("sequence item %d: expected str instance, %s found", i, sentinel_typeName(v))
				}
				parts[i] = s
			}
			return strings.Join(parts, value), nil
		})
	case "startswith", "endswith", "find", "rfind", "index", "rindex", "count":
		return sentinel_native(1, 3, func(a []any) (any, error) {
			sub, err := sentinel_stringArgument(a[0])
			if err != nil {
				return nil, err
			}
			r := []rune(value)
			start, end := 0, len(r)
			if len(a) > 1 {
				start, err = sentinel_integerIndex(a[1])
				if err != nil {
					return nil, err
				}
				if start < 0 {
					start += len(r)
				}
				start = max(0, start)
			}
			if len(a) > 2 {
				end, err = sentinel_integerIndex(a[2])
				if err != nil {
					return nil, err
				}
				if end < 0 {
					end += len(r)
				}
				end = max(0, min(end, len(r)))
			}
			segment := ""
			valid := start <= end && start <= len(r)
			if valid {
				segment = string(r[start:end])
			}
			switch name {
			case "startswith":
				return valid && strings.HasPrefix(segment, sub), nil
			case "endswith":
				return valid && strings.HasSuffix(segment, sub), nil
			case "count":
				if !valid {
					return int64(0), nil
				}
				return int64(strings.Count(segment, sub)), nil
			}
			index := -1
			if valid {
				if name == "rfind" || name == "rindex" {
					index = strings.LastIndex(segment, sub)
				} else {
					index = strings.Index(segment, sub)
				}
			}
			if index < 0 {
				if name == "index" || name == "rindex" {
					return nil, fmt.Errorf("substring not found")
				}
				return int64(-1), nil
			}
			return int64(start + utf8.RuneCountInString(segment[:index])), nil
		})
	case "encode":
		return sentinel_native(0, 2, func(a []any) (any, error) {
			encoding := "utf-8"
			if len(a) > 0 {
				encoding = sentinel_pyStr(a[0])
			}
			switch encoding {
			case "utf-8", "utf8":
				return []byte(value), nil
			case "latin-1", "latin1", "iso-8859-1":
				return sentinel_latin1Bytes(value)
			case "ascii":
				for _, r := range value {
					if r > 127 {
						return nil, fmt.Errorf("ascii codec cannot encode character")
					}
				}
				return []byte(value), nil
			}
			return nil, fmt.Errorf("unknown encoding: %s", encoding)
		})
	case "removeprefix", "removesuffix":
		return sentinel_native(1, 1, func(a []any) (any, error) {
			s, err := sentinel_stringArgument(a[0])
			if err != nil {
				return nil, err
			}
			if name == "removeprefix" {
				return strings.TrimPrefix(value, s), nil
			}
			return strings.TrimSuffix(value, s), nil
		})
	case "isascii", "isalpha", "isalnum", "isdigit", "isdecimal", "isspace", "islower", "isupper":
		return sentinel_native(0, 0, func([]any) (any, error) {
			if name == "isascii" {
				for _, r := range value {
					if r > 127 {
						return false, nil
					}
				}
				return true, nil
			}
			if value == "" {
				return false, nil
			}
			hasCase := false
			for _, r := range value {
				switch name {
				case "isalpha":
					if !unicode.IsLetter(r) {
						return false, nil
					}
				case "isalnum":
					if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
						return false, nil
					}
				case "isdigit", "isdecimal":
					if !unicode.IsDigit(r) {
						return false, nil
					}
				case "isspace":
					if !unicode.IsSpace(r) {
						return false, nil
					}
				case "islower":
					if unicode.IsUpper(r) || unicode.IsTitle(r) {
						return false, nil
					}
					hasCase = hasCase || unicode.IsLower(r)
				case "isupper":
					if unicode.IsLower(r) {
						return false, nil
					}
					hasCase = hasCase || unicode.IsUpper(r)
				}
			}
			if name == "islower" || name == "isupper" {
				return hasCase, nil
			}
			return true, nil
		})
	}
	return nil
}
