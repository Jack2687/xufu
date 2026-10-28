// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	sort "sort"
	utf8 "unicode/utf8"
)

func sentinel_integerIndex(value any) (int, error) {
	i, ok := sentinel_asInt(value)
	if !ok || !i.IsInt64() {
		return 0, fmt.Errorf("'%s' object cannot be interpreted as an integer", sentinel_typeName(value))
	}
	n := i.Int64()
	return int(n), nil
}

func (m *sentinel_machine) lookup(value, key any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if o, ok := value.(*sentinel_object); ok {
		if _, err := sentinel_keyString(key); err != nil {
			return nil, err
		}
		return o.get(key), nil
	}
	if name, ok := key.(string); ok {
		switch v := value.(type) {
		case *sentinel_array:
			if method := m.arrayMethod(v, name); method != nil {
				return method, nil
			}
			return nil, nil
		case string:
			if method := sentinel_stringMethod(v, name); method != nil {
				return sentinel_fn(func(args ...any) (any, error) {
					if err := sentinel_checkStringAllocation(v, name, args, m.limits.valueBytes); err != nil {
						return nil, err
					}
					return method.function(args...)
				}), nil
			}
			return nil, nil
		case []byte:
			if name == "decode" {
				return sentinel_native(0, 2, func(a []any) (any, error) {
					encoding := "utf-8"
					if len(a) > 0 {
						encoding = sentinel_pyStr(a[0])
					}
					switch encoding {
					case "utf-8", "utf8":
						if !utf8.Valid(v) {
							return nil, fmt.Errorf("utf-8 codec cannot decode bytes")
						}
						return string(v), nil
					case "latin-1", "latin1", "iso-8859-1":
						return sentinel_latin1String(v), nil
					case "ascii":
						for _, b := range v {
							if b > 127 {
								return nil, fmt.Errorf("ascii codec cannot decode bytes")
							}
						}
						return string(v), nil
					}
					return nil, fmt.Errorf("unknown encoding: %s", encoding)
				}), nil
			}
		}
		return nil, nil
	}
	n, err := sentinel_integerIndex(key)
	if err != nil {
		return nil, nil
	}
	switch v := value.(type) {
	case *sentinel_array:
		if n < 0 {
			n += len(v.values)
		}
		if n >= 0 && n < len(v.values) {
			return v.values[n], nil
		}
	case string:
		r := []rune(v)
		if n < 0 {
			n += len(r)
		}
		if n >= 0 && n < len(r) {
			return string(r[n]), nil
		}
	case []byte:
		if n < 0 {
			n += len(v)
		}
		if n >= 0 && n < len(v) {
			return int64(v[n]), nil
		}
	}
	return nil, nil
}

func (m *sentinel_machine) arrayMethod(list *sentinel_array, name string) *sentinel_callable {
	guard := func(minArgs, maxArgs int, action func([]any) (any, error)) *sentinel_callable {
		return sentinel_native(minArgs, maxArgs, func(a []any) (any, error) {
			v, err := action(a)
			if err != nil {
				return nil, err
			}
			if err = m.mutate(list); err != nil {
				return nil, err
			}
			return v, nil
		})
	}
	switch name {
	case "append":
		return guard(1, 1, func(a []any) (any, error) { list.values = append(list.values, a[0]); return nil, nil })
	case "clear":
		return guard(0, 0, func([]any) (any, error) { list.values = nil; return nil, nil })
	case "extend":
		return guard(1, 1, func(a []any) (any, error) {
			values, err := sentinel_listCopy(a[0])
			if err != nil {
				return nil, err
			}
			list.values = append(list.values, values.values...)
			return nil, nil
		})
	case "insert":
		return guard(2, 2, func(a []any) (any, error) {
			n, err := sentinel_integerIndex(a[0])
			if err != nil {
				return nil, err
			}
			if n < 0 {
				n += len(list.values)
			}
			n = max(0, min(n, len(list.values)))
			list.values = append(list.values, nil)
			copy(list.values[n+1:], list.values[n:])
			list.values[n] = a[1]
			return nil, nil
		})
	case "pop":
		return guard(0, 1, func(a []any) (any, error) {
			n := -1
			var err error
			if len(a) > 0 {
				n, err = sentinel_integerIndex(a[0])
				if err != nil {
					return nil, err
				}
			}
			if len(list.values) == 0 {
				return nil, fmt.Errorf("pop from empty list")
			}
			if n < 0 {
				n += len(list.values)
			}
			if n < 0 || n >= len(list.values) {
				return nil, fmt.Errorf("pop index out of range")
			}
			value := list.values[n]
			list.values = append(list.values[:n], list.values[n+1:]...)
			return value, nil
		})
	case "remove":
		return guard(1, 1, func(a []any) (any, error) {
			for i, value := range list.values {
				if sentinel_equal(value, a[0]) {
					list.values = append(list.values[:i], list.values[i+1:]...)
					return nil, nil
				}
			}
			return nil, fmt.Errorf("list.remove(x): x not in list")
		})
	case "reverse":
		return guard(0, 0, func([]any) (any, error) {
			for i, j := 0, len(list.values)-1; i < j; i, j = i+1, j-1 {
				list.values[i], list.values[j] = list.values[j], list.values[i]
			}
			return nil, nil
		})
	case "sort":
		return guard(0, 0, func([]any) (any, error) {
			var sortErr error
			sort.SliceStable(list.values, func(i, j int) bool {
				if sortErr != nil {
					return false
				}
				if err := sentinel_checkDeadline(m.options.Deadline); err != nil {
					sortErr = err
					return false
				}
				v, err := sentinel_less(list.values[i], list.values[j])
				if err != nil {
					sortErr = err
				}
				return v
			})
			return nil, sortErr
		})
	case "copy":
		return sentinel_native(0, 0, func([]any) (any, error) { return sentinel_listCopy(list) })
	case "count":
		return sentinel_native(1, 1, func(a []any) (any, error) {
			n := int64(0)
			for _, v := range list.values {
				if sentinel_equal(v, a[0]) {
					n++
				}
			}
			return n, nil
		})
	case "index":
		return sentinel_native(1, 3, func(a []any) (any, error) {
			start, end := 0, len(list.values)
			if len(a) > 1 {
				var err error
				start, err = sentinel_integerIndex(a[1])
				if err != nil {
					return nil, err
				}
				if start < 0 {
					start += len(list.values)
				}
				start = max(0, start)
			}
			if len(a) > 2 {
				var err error
				end, err = sentinel_integerIndex(a[2])
				if err != nil {
					return nil, err
				}
				if end < 0 {
					end += len(list.values)
				}
				end = min(end, len(list.values))
			}
			for i := start; i < end; i++ {
				if sentinel_equal(list.values[i], a[0]) {
					return int64(i), nil
				}
			}
			return nil, fmt.Errorf("%s is not in list", sentinel_pyRepr(a[0], map[any]bool{}))
		})
	}
	return nil
}
