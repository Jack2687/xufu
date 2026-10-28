// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	big "math/big"
	sort "sort"
	strconv "strconv"
	strings "strings"
	unicode "unicode"
)

// 显式引用容器保留 copy、DOM、闭包和寄存器自引用的原有语义。
type sentinel_array struct{ values []any }

func sentinel_arr(values ...any) *sentinel_array { return &sentinel_array{values: values} }

type sentinel_entry struct{ key, value any }
type sentinel_object struct {
	entries []sentinel_entry
	index   map[string]int
}

func sentinel_obj(values ...any) *sentinel_object {
	o := &sentinel_object{index: map[string]int{}}
	for i := 0; i+1 < len(values); i += 2 {
		_ = o.set(values[i], values[i+1])
	}
	return o
}
func (o *sentinel_object) set(key, value any) error {
	k, err := sentinel_keyString(key)
	if err != nil {
		return err
	}
	if i, ok := o.index[k]; ok {
		o.entries[i].value = value
	} else {
		o.index[k] = len(o.entries)
		o.entries = append(o.entries, sentinel_entry{key, value})
	}
	return nil
}
func (o *sentinel_object) get(key any) any {
	k, err := sentinel_keyString(key)
	if err != nil {
		return nil
	}
	if i, ok := o.index[k]; ok {
		return o.entries[i].value
	}
	return nil
}
func (o *sentinel_object) has(key any) bool {
	k, err := sentinel_keyString(key)
	if err != nil {
		return false
	}
	_, ok := o.index[k]
	return ok
}
func (o *sentinel_object) remove(key any) {
	k, err := sentinel_keyString(key)
	if err != nil {
		return
	}
	i, ok := o.index[k]
	if !ok {
		return
	}
	delete(o.index, k)
	o.entries = append(o.entries[:i], o.entries[i+1:]...)
	for j := i; j < len(o.entries); j++ {
		key, _ := sentinel_keyString(o.entries[j].key)
		o.index[key] = j
	}
}

type sentinel_callable struct{ function func(...any) (any, error) }

func sentinel_fn(f func(...any) (any, error)) *sentinel_callable {
	return &sentinel_callable{function: f}
}

func sentinel_keyString(key any) (string, error) {
	switch v := key.(type) {
	case nil:
		return "null", nil
	case string:
		return "s:" + v, nil
	case bool:
		if v {
			return "n:1", nil
		}
		return "n:0", nil
	case int:
		return "n:" + strconv.Itoa(v), nil
	case int64:
		return "n:" + strconv.FormatInt(v, 10), nil
	case *big.Int:
		return "n:" + v.String(), nil
	case float64:
		if r := new(big.Rat).SetFloat64(v); r != nil {
			return "n:" + r.RatString(), nil
		}
		return "n:" + sentinel_pythonFloat(v), nil
	default:
		return "", fmt.Errorf("unhashable type: '%s'", sentinel_typeName(key))
	}
}

func sentinel_typeName(value any) string {
	switch value.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int, int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case *sentinel_array:
		return "list"
	case *sentinel_object:
		return "dict"
	case *sentinel_callable:
		return "function"
	case []byte:
		return "bytes"
	}
	return "object"
}

func sentinel_truth(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int:
		return v != 0
	case int64:
		return v != 0
	case *big.Int:
		return v.Sign() != 0
	case float64:
		return v != 0
	case *sentinel_array:
		return len(v.values) != 0
	case *sentinel_object:
		return len(v.entries) != 0
	case []byte:
		return len(v) != 0
	}
	return true
}

func sentinel_strOrEmpty(value any) string {
	if !sentinel_truth(value) {
		return ""
	}
	return sentinel_pyStr(value)
}
func sentinel_pyStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return sentinel_pyRepr(value, map[any]bool{})
}
func sentinel_pyRepr(value any, seen map[any]bool) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return sentinel_quotePython(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case *big.Int:
		return v.String()
	case float64:
		return sentinel_pythonFloat(v)
	case *sentinel_array:
		if seen[v] {
			return "[...]"
		}
		seen[v] = true
		defer delete(seen, v)
		values := make([]string, len(v.values))
		for i, child := range v.values {
			values[i] = sentinel_pyRepr(child, seen)
		}
		return "[" + strings.Join(values, ", ") + "]"
	case *sentinel_object:
		if seen[v] {
			return "{...}"
		}
		seen[v] = true
		defer delete(seen, v)
		values := make([]string, len(v.entries))
		for i, item := range v.entries {
			values[i] = sentinel_pyRepr(item.key, seen) + ": " + sentinel_pyRepr(item.value, seen)
		}
		return "{" + strings.Join(values, ", ") + "}"
	case []byte:
		return "b" + sentinel_quotePython(sentinel_latin1String(v))
	case *sentinel_callable:
		return "<function>"
	}
	return fmt.Sprint(value)
}

func sentinel_quotePython(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if r == quote {
				b.WriteRune('\\')
				b.WriteRune(r)
			} else if unicode.IsPrint(r) || r == ' ' {
				b.WriteRune(r)
			} else if r < 256 {
				fmt.Fprintf(&b, "\\x%02x", r)
			} else if r <= 0xffff {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				fmt.Fprintf(&b, "\\U%08x", r)
			}
		}
	}
	b.WriteRune(quote)
	return b.String()
}

func sentinel_fromNative(value any) any {
	switch v := value.(type) {
	case []any:
		a := sentinel_arr()
		for _, child := range v {
			a.values = append(a.values, sentinel_fromNative(child))
		}
		return a
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		// 账号 bootstrap 的构造顺序与 Python 客户端固定顺序一致。
		if _, ok := v["session"]; ok {
			for i, k := range keys {
				if k == "session" {
					keys = append([]string{"session"}, append(keys[:i], keys[i+1:]...)...)
					break
				}
			}
		}
		o := sentinel_obj()
		for _, key := range keys {
			_ = o.set(key, sentinel_fromNative(v[key]))
		}
		return o
	case int:
		return int64(v)
	}
	return value
}
