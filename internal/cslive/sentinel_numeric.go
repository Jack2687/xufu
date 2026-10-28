// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	math "math"
	big "math/big"
	strconv "strconv"
	strings "strings"
)

func sentinel_pythonFloat(value float64) string {
	if math.IsNaN(value) {
		return "nan"
	}
	if math.IsInf(value, 1) {
		return "inf"
	}
	if math.IsInf(value, -1) {
		return "-inf"
	}
	e := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.Split(e, "e")
	exp, _ := strconv.Atoi(parts[1])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func sentinel_asFloat(value any) (float64, error) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case *big.Int:
		f, _ := new(big.Float).SetInt(v).Float64()
		if math.IsInf(f, 0) {
			return 0, fmt.Errorf("int too large to convert to float")
		}
		return f, nil
	case float64:
		return v, nil
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(v, "_", ""))
		lower := strings.ToLower(s)
		if lower == "nan" || lower == "+nan" || lower == "-nan" {
			return math.NaN(), nil
		}
		if lower == "infinity" || lower == "+infinity" {
			return math.Inf(1), nil
		}
		if lower == "-infinity" {
			return math.Inf(-1), nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err == nil || math.IsInf(f, 0) {
			return f, nil
		}
		return 0, fmt.Errorf("could not convert string to float: %s", sentinel_quotePython(v))
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", sentinel_typeName(value))
}

func sentinel_asInt(value any) (*big.Int, bool) {
	switch v := value.(type) {
	case int:
		return big.NewInt(int64(v)), true
	case int64:
		return big.NewInt(v), true
	case *big.Int:
		return new(big.Int).Set(v), true
	case bool:
		if v {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return nil, false
}
func sentinel_compactInt(value *big.Int) any {
	if value.IsInt64() {
		return value.Int64()
	}
	return value
}
func sentinel_numericRat(value any) (*big.Rat, bool) {
	if i, ok := sentinel_asInt(value); ok {
		return new(big.Rat).SetInt(i), true
	}
	if f, ok := value.(float64); ok {
		r := new(big.Rat).SetFloat64(f)
		return r, r != nil
	}
	return nil, false
}

func sentinel_equal(a, b any) bool { return sentinel_equalDepth(a, b, 0) }
func sentinel_equalDepth(a, b any, depth int) bool {
	if depth > 64 {
		return false
	}
	if x, ok := sentinel_numericRat(a); ok {
		if y, ok := sentinel_numericRat(b); ok {
			return x.Cmp(y) == 0
		}
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case *sentinel_array:
		y, ok := b.(*sentinel_array)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		if len(x.values) != len(y.values) {
			return false
		}
		for i := range x.values {
			if !sentinel_equalDepth(x.values[i], y.values[i], depth+1) {
				return false
			}
		}
		return true
	case *sentinel_object:
		y, ok := b.(*sentinel_object)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		if len(x.entries) != len(y.entries) {
			return false
		}
		for _, item := range x.entries {
			if !y.has(item.key) || !sentinel_equalDepth(item.value, y.get(item.key), depth+1) {
				return false
			}
		}
		return true
	case *sentinel_callable:
		y, ok := b.(*sentinel_callable)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	}
	return false
}

func sentinel_less(a, b any) (bool, error) { return sentinel_lessDepth(a, b, 0) }
func sentinel_lessDepth(a, b any, depth int) (bool, error) {
	if depth > 64 {
		return false, fmt.Errorf("maximum recursion depth exceeded in comparison")
	}
	if x, ok := sentinel_numericRat(a); ok {
		if y, ok := sentinel_numericRat(b); ok {
			return x.Cmp(y) < 0, nil
		}
	}
	if sentinel_isNumber(a) && sentinel_isNumber(b) {
		if x, ok := a.(float64); ok {
			if math.IsNaN(x) {
				return false, nil
			}
			if math.IsInf(x, 0) {
				if y, ok := b.(float64); ok {
					return x < y, nil
				}
				return math.IsInf(x, -1), nil
			}
		}
		if y, ok := b.(float64); ok {
			if math.IsNaN(y) {
				return false, nil
			}
			if math.IsInf(y, 0) {
				return math.IsInf(y, 1), nil
			}
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return x < y, nil
		}
	}
	if x, ok := a.(*sentinel_array); ok {
		if y, ok := b.(*sentinel_array); ok {
			for i := 0; i < min(len(x.values), len(y.values)); i++ {
				if !sentinel_equal(x.values[i], y.values[i]) {
					return sentinel_lessDepth(x.values[i], y.values[i], depth+1)
				}
			}
			return len(x.values) < len(y.values), nil
		}
	}
	return false, fmt.Errorf("'<' not supported between instances of '%s' and '%s'", sentinel_typeName(a), sentinel_typeName(b))
}

func sentinel_subtract(a, b any) (any, error) {
	if x, ok := sentinel_asInt(a); ok {
		if y, ok := sentinel_asInt(b); ok {
			return sentinel_compactInt(new(big.Int).Sub(x, y)), nil
		}
	}
	if sentinel_isNumber(a) && sentinel_isNumber(b) {
		x, err := sentinel_asFloat(a)
		if err != nil {
			return nil, err
		}
		y, err := sentinel_asFloat(b)
		if err != nil {
			return nil, err
		}
		return x - y, nil
	}
	return nil, fmt.Errorf("unsupported operand type(s) for -: '%s' and '%s'", sentinel_typeName(a), sentinel_typeName(b))
}

func sentinel_isNumber(value any) bool {
	switch value.(type) {
	case int, int64, *big.Int, bool, float64:
		return true
	}
	return false
}
