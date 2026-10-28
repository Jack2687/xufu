// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	errors "errors"
	fmt "fmt"
	math "math"
	time "time"
	utf8 "unicode/utf8"
)

const (
	sentinel_maxInstructions   = 20000
	sentinel_maxExecutionDepth = 32
	sentinel_maxDataDepth      = 64
	sentinel_maxValueBytes     = 256 * 1024
	sentinel_maxContainerItems = 100000
	sentinel_maxRegisterBytes  = 4 * 1024 * 1024
	sentinel_maxDXBytes        = 1024 * 1024
	sentinel_maxRegexBytes     = 4 * 1024
)

type sentinel_Options struct {
	Deadline  time.Time
	Profile   protocol_BrowserProfile
	Cookies   string
	Clock     fingerprint_Clock
	Bootstrap map[string]any
}

type sentinel_limits struct{ instructions, executionDepth, dataDepth, valueBytes, containerItems, registerBytes, dxBytes, regexBytes int }

func sentinel_defaultLimits() sentinel_limits {
	return sentinel_limits{sentinel_maxInstructions, sentinel_maxExecutionDepth, sentinel_maxDataDepth, sentinel_maxValueBytes, sentinel_maxContainerItems, sentinel_maxRegisterBytes, sentinel_maxDXBytes, sentinel_maxRegexBytes}
}

type sentinel_size struct{ bytes, items int }

func (a sentinel_size) add(b sentinel_size) sentinel_size {
	return sentinel_size{a.bytes + b.bytes, a.items + b.items}
}
func (a sentinel_size) sub(b sentinel_size) sentinel_size {
	return sentinel_size{a.bytes - b.bytes, a.items - b.items}
}

type sentinel_machine struct {
	options          sentinel_Options
	limits           sentinel_limits
	regs             *sentinel_object
	registerSizes    map[string]sentinel_size
	registerTotal    sentinel_size
	mutationSizes    map[any]sentinel_size
	mutationTotal    sentinel_size
	window           *sentinel_object
	success          *string
	remaining, depth int
}

func sentinel_SolveTurnstileDX(dx, requirements string, options sentinel_Options) (string, error) {
	return sentinel_solveWithLimits(dx, requirements, options, sentinel_defaultLimits())
}
func sentinel_solveWithLimits(dx, requirements string, options sentinel_Options, limits sentinel_limits) (token string, err error) {
	// 解析/方法调用出现普通错误时对外只返回稳定码，保留本地错误链。
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &sentinel_Error{Code: "sentinel_turnstile_error", Cause: fmt.Errorf("vm execution failed")}
			token = ""
		}
		if err != nil {
			var protocol *sentinel_Error
			if !errors.As(err, &protocol) {
				err = &sentinel_Error{Code: "sentinel_turnstile_error", Cause: err}
			}
		}
	}()
	if err = sentinel_checkDeadline(options.Deadline); err != nil {
		return "", err
	}
	if utf8.RuneCountInString(dx) > limits.dxBytes {
		return "", sentinel_resourceError("turnstile_dx_limit")
	}
	decoded, err := sentinel_b64DecodeLatin1(dx)
	if err != nil {
		return "", err
	}
	decoded = sentinel_xorLatin1(decoded, requirements)
	if utf8.RuneCountInString(decoded) > limits.dxBytes {
		return "", sentinel_resourceError("turnstile_dx_limit")
	}
	if err = sentinel_checkDeadline(options.Deadline); err != nil {
		return "", err
	}
	program, err := sentinel_parseJSON(decoded)
	if err != nil {
		return "", err
	}
	m := &sentinel_machine{options: options, limits: limits, regs: sentinel_obj(), registerSizes: map[string]sentinel_size{}, mutationSizes: map[any]sentinel_size{}, remaining: limits.instructions}
	if _, err = m.measure(program); err != nil {
		return "", err
	}
	m.window = m.buildWindow()
	if err = m.initialize(requirements, program); err != nil {
		return "", err
	}
	if err = m.run(); err != nil {
		return "", err
	}
	if m.success == nil || *m.success == "" {
		return "", sentinel_resourceError("turnstile_result_missing")
	}
	if err = sentinel_checkDeadline(options.Deadline); err != nil {
		return "", err
	}
	return *m.success, nil
}

func (m *sentinel_machine) measure(value any) (sentinel_size, error) {
	seen := map[any]bool{}
	nodes, items := 0, 0
	var walk func(any, int) (int, error)
	walk = func(value any, depth int) (int, error) {
		nodes++
		if nodes%128 == 1 {
			if err := sentinel_checkDeadline(m.options.Deadline); err != nil {
				return 0, err
			}
		}
		if depth > m.limits.dataDepth {
			return 0, sentinel_resourceError("turnstile_data_depth_limit")
		}
		switch v := value.(type) {
		case string:
			if !utf8.ValidString(v) {
				return 0, fmt.Errorf("invalid UTF-8 string")
			}
			if len(v) > m.limits.valueBytes {
				return 0, sentinel_resourceError("turnstile_value_limit")
			}
			return len(v), nil
		case []byte:
			if len(v) > m.limits.valueBytes {
				return 0, sentinel_resourceError("turnstile_value_limit")
			}
			return len(v), nil
		case *sentinel_array:
			if seen[v] {
				return 0, nil
			}
			seen[v] = true
			items += len(v.values)
			if items > m.limits.containerItems {
				return 0, sentinel_resourceError("turnstile_container_limit")
			}
			total := len(v.values)
			for _, child := range v.values {
				n, err := walk(child, depth+1)
				if err != nil {
					return 0, err
				}
				total += n
				if total > m.limits.registerBytes {
					return 0, sentinel_resourceError("turnstile_register_limit")
				}
			}
			return total, nil
		case *sentinel_object:
			if seen[v] {
				return 0, nil
			}
			seen[v] = true
			items += len(v.entries)
			if items > m.limits.containerItems {
				return 0, sentinel_resourceError("turnstile_container_limit")
			}
			total := len(v.entries)
			for _, child := range v.entries {
				for _, part := range []any{child.key, child.value} {
					n, err := walk(part, depth+1)
					if err != nil {
						return 0, err
					}
					total += n
				}
				if total > m.limits.registerBytes {
					return 0, sentinel_resourceError("turnstile_register_limit")
				}
			}
			return total, nil
		}
		return 1, nil
	}
	bytes, err := walk(value, 0)
	if err != nil {
		return sentinel_size{}, err
	}
	if err = sentinel_checkDeadline(m.options.Deadline); err != nil {
		return sentinel_size{}, err
	}
	return sentinel_size{bytes, items}, nil
}

func (m *sentinel_machine) checkSize(s sentinel_size) error {
	if s.bytes > m.limits.registerBytes {
		return sentinel_resourceError("turnstile_register_limit")
	}
	if s.items > m.limits.containerItems {
		return sentinel_resourceError("turnstile_container_limit")
	}
	return nil
}
func (m *sentinel_machine) set(key, value any) error {
	s, err := m.measure(value)
	if err != nil {
		return err
	}
	k, err := sentinel_keyString(key)
	if err != nil {
		return err
	}
	updated := m.registerTotal.sub(m.registerSizes[k]).add(s)
	if err = m.checkSize(updated.add(m.mutationTotal)); err != nil {
		return err
	}
	if err = m.regs.set(key, value); err != nil {
		return err
	}
	m.registerSizes[k] = s
	m.registerTotal = updated
	return nil
}
func (m *sentinel_machine) get(key any) any { return m.regs.get(key) }
func (m *sentinel_machine) refresh(key any) error {
	if m.regs.has(key) {
		return m.set(key, m.get(key))
	}
	return nil
}
func (m *sentinel_machine) mutate(value any) error {
	s, err := m.measure(value)
	if err != nil {
		return err
	}
	switch value.(type) {
	case *sentinel_object, *sentinel_array:
	default:
		return nil
	}
	updated := m.mutationTotal.sub(m.mutationSizes[value]).add(s)
	if err = m.checkSize(m.registerTotal.add(updated)); err != nil {
		return err
	}
	m.mutationSizes[value] = s
	m.mutationTotal = updated
	return nil
}

func (m *sentinel_machine) initialize(requirements string, program any) error {
	var initErr error
	handle := func(opcode, minArgs, maxArgs int, handler func([]any) (any, error)) {
		if initErr != nil {
			return
		}
		initErr = m.set(int64(opcode), sentinel_fn(func(args ...any) (any, error) {
			if len(args) < minArgs || (maxArgs >= 0 && len(args) > maxArgs) {
				return nil, fmt.Errorf("invalid argument count")
			}
			return handler(args)
		}))
	}
	noResult := func(err error) (any, error) { return nil, err }
	noop := func([]any) (any, error) { return nil, nil }
	handle(0, 1, 1, noop)
	handle(1, 2, 2, func(a []any) (any, error) {
		return noResult(m.set(a[0], sentinel_xorLatin1(sentinel_strOrEmpty(m.get(a[0])), sentinel_strOrEmpty(m.get(a[1])))))
	})
	handle(2, 2, 2, func(a []any) (any, error) { return noResult(m.set(a[0], a[1])) })
	encodeResult := func(value any) (string, error) {
		raw := sentinel_pyStr(value)
		if _, err := m.measure(raw); err != nil {
			return "", err
		}
		encoded, err := sentinel_b64EncodeLatin1(raw)
		if err != nil {
			return "", err
		}
		if _, err = m.measure(encoded); err != nil {
			return "", err
		}
		return encoded, nil
	}
	handle(3, 1, 1, func(a []any) (any, error) {
		s, err := encodeResult(a[0])
		if err != nil {
			return nil, err
		}
		m.success = &s
		return nil, nil
	})
	handle(4, 1, 1, func(a []any) (any, error) {
		s, err := encodeResult(a[0])
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s", s)
	})
	handle(5, 2, 2, func(a []any) (any, error) {
		cur, add := m.get(a[0]), m.get(a[1])
		if list, ok := cur.(*sentinel_array); ok {
			if len(list.values) >= m.limits.containerItems {
				return nil, sentinel_resourceError("turnstile_container_limit")
			}
			list.values = append(list.values, add)
			return noResult(m.refresh(a[0]))
		}
		text := ""
		if add != nil {
			text = sentinel_pyStr(add)
		}
		return noResult(m.set(a[0], sentinel_strOrEmpty(cur)+text))
	})
	handle(6, 3, 3, func(a []any) (any, error) {
		v, err := m.lookup(m.get(a[1]), m.get(a[2]))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(7, 1, -1, func(a []any) (any, error) {
		f, ok := m.get(a[0]).(*sentinel_callable)
		if !ok {
			return nil, fmt.Errorf("call non-function %s", sentinel_pyStr(a[0]))
		}
		return f.function(m.dereference(a[1:])...)
	})
	copyRegister := func(a []any) (any, error) { return noResult(m.set(a[0], m.get(a[1]))) }
	handle(8, 2, 2, copyRegister)
	if initErr == nil {
		initErr = m.set(int64(10), m.window)
	}
	handle(11, 2, 2, func(a []any) (any, error) {
		v, err := m.findScript(sentinel_strOrEmpty(m.get(a[1])))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(12, 1, 1, func(a []any) (any, error) { return noResult(m.set(a[0], m.regs)) })
	handle(13, 2, -1, func(a []any) (any, error) {
		if f, ok := m.get(a[1]).(*sentinel_callable); ok {
			_, err := f.function(a[2:]...)
			if err != nil {
				if sentinel_isProtocol(err) {
					return nil, err
				}
				return noResult(m.set(a[0], err.Error()))
			}
		}
		return nil, nil
	})
	handle(14, 2, 2, func(a []any) (any, error) {
		v, err := sentinel_parseJSON(sentinel_strOrEmpty(m.get(a[1])))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(15, 2, 2, func(a []any) (any, error) {
		v, err := sentinel_dumpJSON(m.get(a[1]), true, true)
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(17, 2, -1, func(a []any) (any, error) {
		var v any
		var err error
		if f, ok := m.get(a[1]).(*sentinel_callable); ok {
			v, err = f.function(m.dereference(a[2:])...)
		}
		if err != nil {
			if sentinel_isProtocol(err) {
				return nil, err
			}
			v = err.Error()
		}
		return noResult(m.set(a[0], v))
	})
	handle(18, 1, 1, func(a []any) (any, error) {
		v, err := sentinel_b64DecodeLatin1(sentinel_strOrEmpty(m.get(a[0])))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(19, 1, 1, func(a []any) (any, error) {
		v, err := sentinel_b64EncodeLatin1(sentinel_strOrEmpty(m.get(a[0])))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(20, 3, -1, func(a []any) (any, error) {
		if sentinel_equal(m.get(a[0]), m.get(a[1])) {
			if f, ok := m.get(a[2]).(*sentinel_callable); ok {
				return f.function(a[3:]...)
			}
		}
		return nil, nil
	})
	handle(21, 4, -1, func(a []any) (any, error) {
		diff, err := sentinel_subtract(m.get(a[0]), m.get(a[1]))
		if err != nil {
			return nil, nil
		}
		var absolute any
		if n, ok := sentinel_asInt(diff); ok {
			absolute = sentinel_compactInt(n.Abs(n))
		} else if n, ok := diff.(float64); ok {
			absolute = math.Abs(n)
		} else {
			return nil, nil
		}
		r, err := sentinel_less(m.get(a[2]), absolute)
		if err != nil {
			return nil, nil
		}
		if r {
			if f, ok := m.get(a[3]).(*sentinel_callable); ok {
				v, err := f.function(a[4:]...)
				if err != nil && !sentinel_isProtocol(err) {
					return nil, nil
				}
				return v, err
			}
		}
		return nil, nil
	})
	handle(22, 2, 2, func(a []any) (any, error) {
		prev := m.copyQueue()
		body, err := sentinel_listCopy(a[1])
		if err != nil {
			return nil, err
		}
		if err = m.set(int64(9), body); err != nil {
			return nil, err
		}
		err = m.run()
		if err != nil && !sentinel_isProtocol(err) {
			err = m.set(a[0], err.Error())
		}
		restore := m.set(int64(9), prev)
		if restore != nil {
			return nil, restore
		}
		return nil, err
	})
	handle(23, 2, -1, func(a []any) (any, error) {
		if m.get(a[0]) != nil {
			if f, ok := m.get(a[1]).(*sentinel_callable); ok {
				return f.function(a[2:]...)
			}
		}
		return nil, nil
	})
	handle(24, 3, 3, func(a []any) (any, error) {
		v, err := m.lookup(m.get(a[1]), m.get(a[2]))
		if err != nil {
			return nil, err
		}
		f, ok := v.(*sentinel_callable)
		if !ok {
			return nil, fmt.Errorf("bind fail key=%s", sentinel_pyStr(m.get(a[2])))
		}
		return noResult(m.set(a[0], sentinel_fn(f.function)))
	})
	for _, opcode := range []int{25, 26} {
		handle(opcode, 0, -1, noop)
	}
	handle(27, 2, 2, func(a []any) (any, error) {
		cur, value := m.get(a[0]), m.get(a[1])
		if list, ok := cur.(*sentinel_array); ok {
			for i, item := range list.values {
				if sentinel_equal(item, value) {
					list.values = append(list.values[:i], list.values[i+1:]...)
					break
				}
			}
			return noResult(m.refresh(a[0]))
		}
		v, err := sentinel_subtract(cur, value)
		if err != nil {
			v = nil
		}
		return noResult(m.set(a[0], v))
	})
	handle(28, 0, -1, noop)
	handle(29, 3, 3, func(a []any) (any, error) {
		v, err := sentinel_less(m.get(a[1]), m.get(a[2]))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], v))
	})
	handle(30, 4, 4, func(a []any) (any, error) {
		target, ret := a[0], a[1]
		params := sentinel_arr()
		var body any = a[2]
		if r, ok := a[3].(*sentinel_array); ok {
			var ok bool
			params, ok = a[2].(*sentinel_array)
			if !ok {
				return nil, fmt.Errorf("parameter list is not iterable")
			}
			body = r
		} else if !sentinel_truth(body) {
			body = sentinel_arr()
		}
		return noResult(m.set(target, sentinel_fn(func(args ...any) (result any, err error) {
			prev := m.copyQueue()
			for i, param := range params.values {
				if i < len(args) {
					if err = m.set(param, args[i]); err != nil {
						return nil, err
					}
				}
			}
			queue, err := sentinel_listCopy(body)
			if err != nil {
				return nil, err
			}
			if err = m.set(int64(9), queue); err != nil {
				return nil, err
			}
			defer func() {
				if restore := m.set(int64(9), prev); restore != nil {
					err = restore
				}
			}()
			if err = m.run(); err != nil {
				return nil, err
			}
			return m.get(ret), nil
		})))
	})
	handle(33, 3, 3, func(a []any) (any, error) {
		x, err := sentinel_asFloat(m.get(a[1]))
		if err != nil {
			return nil, err
		}
		y, err := sentinel_asFloat(m.get(a[2]))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], x*y))
	})
	handle(34, 2, 2, copyRegister)
	handle(35, 3, 3, func(a []any) (any, error) {
		denom := m.get(a[2])
		if !sentinel_truth(denom) {
			denom = int64(0)
		}
		d, err := sentinel_asFloat(denom)
		if err != nil {
			return nil, err
		}
		if d == 0 {
			return noResult(m.set(a[0], int64(0)))
		}
		n, err := sentinel_asFloat(m.get(a[1]))
		if err != nil {
			return nil, err
		}
		return noResult(m.set(a[0], n/d))
	})
	if initErr != nil {
		return initErr
	}
	if err := m.set(int64(16), requirements); err != nil {
		return err
	}
	return m.set(int64(9), program)
}

func sentinel_isProtocol(err error) bool {
	var protocol *sentinel_Error
	return errors.As(err, &protocol)
}
func (m *sentinel_machine) dereference(args []any) []any {
	values := make([]any, len(args))
	for i, a := range args {
		values[i] = m.get(a)
	}
	return values
}
func sentinel_listCopy(value any) (*sentinel_array, error) {
	switch v := value.(type) {
	case *sentinel_array:
		return sentinel_arr(append([]any(nil), v.values...)...), nil
	case *sentinel_object:
		a := sentinel_arr()
		for _, e := range v.entries {
			a.values = append(a.values, e.key)
		}
		return a, nil
	case string:
		a := sentinel_arr()
		for _, r := range v {
			a.values = append(a.values, string(r))
		}
		return a, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", sentinel_typeName(value))
}
func (m *sentinel_machine) copyQueue() *sentinel_array {
	v := m.get(int64(9))
	if !sentinel_truth(v) {
		return sentinel_arr()
	}
	a, _ := sentinel_listCopy(v)
	if a == nil {
		return sentinel_arr()
	}
	return a
}
func (m *sentinel_machine) run() error {
	if m.depth >= m.limits.executionDepth {
		return sentinel_resourceError("turnstile_execution_depth_limit")
	}
	m.depth++
	defer func() { m.depth-- }()
	for {
		if err := sentinel_checkDeadline(m.options.Deadline); err != nil {
			return err
		}
		queue, ok := m.get(int64(9)).(*sentinel_array)
		if !ok || len(queue.values) == 0 {
			return nil
		}
		instr := queue.values[0]
		queue.values = queue.values[1:]
		a, ok := instr.(*sentinel_array)
		if !ok || len(a.values) == 0 {
			continue
		}
		opcode := a.values[0]
		handler := m.get(opcode)
		f, ok := handler.(*sentinel_callable)
		if !ok {
			return fmt.Errorf("opcode %s is %s", sentinel_pyStr(opcode), sentinel_typeName(handler))
		}
		m.remaining--
		if m.remaining < 0 {
			return sentinel_resourceError("turnstile_instruction_limit")
		}
		if _, err := f.function(a.values[1:]...); err != nil {
			return err
		}
		if err := sentinel_checkDeadline(m.options.Deadline); err != nil {
			return err
		}
		if m.success != nil {
			return nil
		}
	}
}
