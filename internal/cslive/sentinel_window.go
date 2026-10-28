// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	math "math"
	big "math/big"
	dep_math_rand_v2 "math/rand/v2"
)

func sentinel_native(minArgs, maxArgs int, f func([]any) (any, error)) *sentinel_callable {
	return sentinel_fn(func(args ...any) (any, error) {
		if len(args) < minArgs || (maxArgs >= 0 && len(args) > maxArgs) {
			return nil, fmt.Errorf("invalid argument count")
		}
		return f(args)
	})
}
func sentinel_constant(value any) *sentinel_callable {
	return sentinel_native(0, -1, func([]any) (any, error) { return value, nil })
}

func (m *sentinel_machine) element(tag any) *sentinel_object {
	name := sentinel_strOrEmpty(tag)
	if name == "" {
		name = "DIV"
	}
	name = sentinel_upper(name)
	el := sentinel_obj("nodeName", name, "tagName", name, "style", sentinel_obj(), "attributes", sentinel_obj(), "children", sentinel_arr(), "parentNode", nil, "ownerDocument", nil)
	attributes := el.get("attributes").(*sentinel_object)
	children := el.get("children").(*sentinel_array)
	_ = el.set("setAttribute", sentinel_native(2, 2, func(a []any) (any, error) {
		if err := attributes.set(sentinel_pyStr(a[0]), sentinel_pyStr(a[1])); err != nil {
			return nil, err
		}
		return nil, m.mutate(attributes)
	}))
	_ = el.set("getAttribute", sentinel_native(1, 1, func(a []any) (any, error) { return attributes.get(a[0]), nil }))
	_ = el.set("appendChild", sentinel_native(1, 1, func(a []any) (any, error) {
		children.values = append(children.values, a[0])
		if child, ok := a[0].(*sentinel_object); ok {
			_ = child.set("parentNode", el)
		}
		if err := m.mutate(children); err != nil {
			return nil, err
		}
		return a[0], nil
	}))
	_ = el.set("removeChild", sentinel_native(1, 1, func(a []any) (any, error) {
		items := children.values[:0]
		for _, child := range children.values {
			if !sentinel_identical(child, a[0]) {
				items = append(items, child)
			}
		}
		children.values = items
		if err := m.mutate(children); err != nil {
			return nil, err
		}
		return a[0], nil
	}))
	_ = el.set("remove", sentinel_native(0, 0, func([]any) (any, error) {
		if parent, ok := el.get("parentNode").(*sentinel_object); ok {
			if kids, ok := parent.get("children").(*sentinel_array); ok {
				items := kids.values[:0]
				for _, child := range kids.values {
					if !sentinel_identical(child, el) {
						items = append(items, child)
					}
				}
				kids.values = items
				return nil, m.mutate(kids)
			}
		}
		return nil, nil
	}))
	_ = el.set("click", sentinel_native(0, 0, func([]any) (any, error) { return nil, nil }))
	_ = el.set("addEventListener", sentinel_constant(nil))
	_ = el.set("removeEventListener", sentinel_constant(nil))
	_ = el.set("getBoundingClientRect", sentinel_native(0, 0, func([]any) (any, error) {
		return sentinel_obj("x", int64(0), "y", int64(0), "width", int64(0), "height", int64(0), "top", int64(0), "left", int64(0), "right", int64(0), "bottom", int64(0)), nil
	}))
	return el
}

func sentinel_identical(a, b any) bool {
	switch x := a.(type) {
	case *sentinel_object:
		y, ok := b.(*sentinel_object)
		return ok && x == y
	case *sentinel_array:
		y, ok := b.(*sentinel_array)
		return ok && x == y
	case *sentinel_callable:
		y, ok := b.(*sentinel_callable)
		return ok && x == y
	case nil:
		return b == nil
	}
	return sentinel_equal(a, b)
}

func (m *sentinel_machine) buildWindow() *sentinel_object {
	document := sentinel_obj()
	body, html := m.element("body"), m.element("html")
	_ = document.set("scripts", sentinel_arr(sentinel_obj("src", sentinel_SDKStubURL), sentinel_obj("src", sentinel_SDKURL)))
	_ = document.set("body", body)
	_ = document.set("documentElement", html)
	_ = document.set("cookie", m.options.Cookies)
	_ = document.set("createElement", sentinel_native(1, 1, func(a []any) (any, error) { return m.element(a[0]), nil }))
	_ = document.set("getElementById", sentinel_constant(nil))
	_ = document.set("querySelector", sentinel_constant(nil))
	_ = document.set("querySelectorAll", sentinel_native(0, -1, func([]any) (any, error) { return sentinel_arr(), nil }))
	_ = body.set("ownerDocument", document)
	_ = html.set("ownerDocument", document)
	storage := sentinel_obj()
	storageAPI := sentinel_obj(
		"getItem", sentinel_native(1, 1, func(a []any) (any, error) { return storage.get(sentinel_pyStr(a[0])), nil }),
		"setItem", sentinel_native(2, 2, func(a []any) (any, error) {
			_ = storage.set(sentinel_pyStr(a[0]), sentinel_pyStr(a[1]))
			return nil, m.mutate(storage)
		}),
		"removeItem", sentinel_native(1, 1, func(a []any) (any, error) { storage.remove(sentinel_pyStr(a[0])); return nil, m.mutate(storage) }),
	)
	reflectSet := func(a []any) (any, error) {
		target, ok := a[0].(*sentinel_object)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute '%s'", sentinel_typeName(a[0]), sentinel_pyStr(a[1]))
		}
		if err := target.set(a[1], a[2]); err != nil {
			return nil, err
		}
		if err := m.mutate(target); err != nil {
			return nil, err
		}
		return true, nil
	}
	profile := m.options.Profile
	var pixelRatio any = profile.DevicePixelRatio
	if profile.DevicePixelRatio == math.Trunc(profile.DevicePixelRatio) {
		pixelRatio = int64(profile.DevicePixelRatio)
	}
	bootstrap := sentinel_fromNative(m.options.Bootstrap)
	if bootstrap == nil {
		bootstrap = sentinel_obj()
	}
	window := sentinel_obj(
		"document", document,
		"navigator", sentinel_obj("userAgent", profile.UserAgent, "language", "en-US", "languages", sentinel_arr("en-US", "en"), "hardwareConcurrency", int64(profile.HardwareConcurrency), "platform", profile.NavigatorPlatform, "vendor", "Google Inc.", "webdriver", false),
		"screen", sentinel_obj("width", int64(profile.ScreenWidth), "height", int64(profile.ScreenHeight), "availWidth", int64(profile.AvailWidth), "availHeight", int64(profile.AvailHeight), "colorDepth", int64(profile.ColorDepth), "pixelDepth", int64(profile.ColorDepth)),
		"history", sentinel_obj("length", int64(2), "scrollRestoration", "auto"),
		"location", sentinel_obj("href", "https://chatgpt.com/", "search", "", "pathname", "/", "origin", "https://chatgpt.com", "host", "chatgpt.com", "protocol", "https:"),
		"localStorage", storageAPI,
		"performance", sentinel_obj("now", sentinel_native(0, 0, func([]any) (any, error) { return m.options.Clock.NowMS(), nil }), "memory", sentinel_obj("jsHeapSizeLimit", int64(4294705152), "totalJSHeapSize", int64(10000000), "usedJSHeapSize", int64(5000000))),
		"Reflect", sentinel_obj("set", sentinel_native(3, -1, reflectSet), "get", sentinel_native(2, -1, func(a []any) (any, error) {
			if target, ok := a[0].(*sentinel_object); ok {
				return target.get(a[1]), nil
			}
			return m.lookup(a[0], sentinel_pyStr(a[1]))
		})),
		"Object", sentinel_obj("create", sentinel_native(0, -1, func([]any) (any, error) { return sentinel_obj(), nil }), "keys", sentinel_native(1, 1, func(a []any) (any, error) {
			keys := sentinel_arr()
			if o, ok := a[0].(*sentinel_object); ok {
				for _, e := range o.entries {
					keys.values = append(keys.values, e.key)
				}
			}
			return keys, nil
		}), "assign", sentinel_native(1, -1, func(a []any) (any, error) { return a[0], nil }), "set", sentinel_native(3, 3, func(a []any) (any, error) { _, err := reflectSet(a); return nil, err })),
		"Math", m.mathObject(),
		"JSON", sentinel_obj("parse", sentinel_native(1, 1, func(a []any) (any, error) {
			s, ok := a[0].(string)
			if !ok {
				return nil, fmt.Errorf("the JSON object must be str, bytes or bytearray, not %s", sentinel_typeName(a[0]))
			}
			return sentinel_parseJSON(s)
		}), "stringify", sentinel_native(1, 1, func(a []any) (any, error) { return sentinel_dumpJSON(a[0], true, true) })),
		"innerWidth", int64(profile.ScreenWidth), "innerHeight", int64(profile.ScreenHeight), "devicePixelRatio", pixelRatio, "chrome", sentinel_obj("runtime", sentinel_obj()),
		"__reactRouterContext", sentinel_obj("state", sentinel_obj("loaderData", sentinel_obj("root", sentinel_obj("clientBootstrap", bootstrap)))),
		"atob", sentinel_native(1, 1, func(a []any) (any, error) {
			s, ok := a[0].(string)
			if !ok {
				return nil, fmt.Errorf("encoding expects str")
			}
			return sentinel_b64DecodeLatin1(s)
		}),
		"btoa", sentinel_native(1, 1, func(a []any) (any, error) {
			s, ok := a[0].(string)
			if !ok {
				return nil, fmt.Errorf("encoding expects str")
			}
			return sentinel_b64EncodeLatin1(s)
		}),
	)
	_ = window.set("window", window)
	return window
}

func (m *sentinel_machine) mathObject() *sentinel_object {
	abs := sentinel_native(1, 1, func(a []any) (any, error) {
		if i, ok := sentinel_asInt(a[0]); ok {
			return sentinel_compactInt(new(big.Int).Abs(i)), nil
		}
		if f, ok := a[0].(float64); ok {
			return math.Abs(f), nil
		}
		return nil, fmt.Errorf("bad operand type for abs(): '%s'", sentinel_typeName(a[0]))
	})
	floor := sentinel_native(1, 1, func(a []any) (any, error) {
		if i, ok := sentinel_asInt(a[0]); ok {
			return sentinel_compactInt(i), nil
		}
		f, ok := a[0].(float64)
		if !ok {
			return nil, fmt.Errorf("unsupported floor operand")
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("cannot convert float to integer")
		}
		i, _ := new(big.Float).SetFloat64(math.Floor(f)).Int(nil)
		return sentinel_compactInt(i), nil
	})
	round := sentinel_native(1, 2, func(a []any) (any, error) {
		if len(a) == 2 {
			places, err := sentinel_integerIndex(a[1])
			if err != nil {
				return nil, err
			}
			f, err := sentinel_asFloat(a[0])
			if err != nil {
				return nil, err
			}
			if places >= 0 && places <= 308 {
				return sentinel_roundDecimal(f, places), nil
			}
			return nil, fmt.Errorf("unsupported rounding precision")
		}
		if i, ok := sentinel_asInt(a[0]); ok {
			return sentinel_compactInt(i), nil
		}
		f, err := sentinel_asFloat(a[0])
		if err != nil {
			return nil, err
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("cannot convert float to integer")
		}
		i, _ := new(big.Float).SetFloat64(math.RoundToEven(f)).Int(nil)
		return sentinel_compactInt(i), nil
	})
	minMax := func(maximum bool) *sentinel_callable {
		return sentinel_native(1, -1, func(a []any) (any, error) {
			values := a
			if len(a) == 1 {
				v, err := sentinel_listCopy(a[0])
				if err != nil {
					return nil, err
				}
				values = v.values
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("arg is an empty sequence")
			}
			best := values[0]
			for _, value := range values[1:] {
				var take bool
				var err error
				if maximum {
					take, err = sentinel_less(best, value)
				} else {
					take, err = sentinel_less(value, best)
				}
				if err != nil {
					return nil, err
				}
				if take {
					best = value
				}
			}
			return best, nil
		})
	}
	return sentinel_obj("abs", abs, "floor", floor, "round", round, "random", sentinel_native(0, 0, func([]any) (any, error) { return dep_math_rand_v2.Float64(), nil }), "max", minMax(true), "min", minMax(false))
}
