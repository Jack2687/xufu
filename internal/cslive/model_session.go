// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	json "encoding/json"
	errors "errors"
	fmt "fmt"
	strings "strings"
	utf8 "unicode/utf8"
)

const model_SessionJSONMaxChars = 262144

type model_Session struct {
	AccessToken  string `json:"access_token"`
	SessionToken string `json:"session_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Cookies      string `json:"cookies"`
}

func (s model_Session) String() string { return "Session{redacted}" }

func (s model_Session) GoString() string { return s.String() }

func model_ParseSession(raw string) (model_Session, error) {
	if !utf8.ValidString(raw) || utf8.RuneCountInString(raw) > model_SessionJSONMaxChars {
		return model_Session{}, fmt.Errorf("invalid Session JSON input")
	}
	m, e := model_decodeSession(raw)
	if e != nil {
		return model_Session{}, e
	}
	text := func(m protocol_Map, k string, required bool) (string, error) {
		v, ok := m[k]
		if !ok {
			v = ""
		}
		s, ok := v.(string)
		if !ok || required && strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("invalid Session field: %s", k)
		}
		return strings.TrimSpace(s), nil
	}
	nested := func(k string) (protocol_Map, error) {
		v, ok := m[k]
		if !ok {
			return protocol_Map{}, nil
		}
		obj, ok := v.(protocol_Map)
		if !ok {
			return nil, fmt.Errorf("Session account and user must be objects")
		}
		return obj, nil
	}
	a, e := nested("account")
	if e != nil {
		return model_Session{}, e
	}
	u, e := nested("user")
	if e != nil {
		return model_Session{}, e
	}
	s := model_Session{}
	for _, p := range []struct {
		m        protocol_Map
		k        string
		dst      *string
		required bool
	}{{m, "accessToken", &s.AccessToken, true}, {m, "sessionToken", &s.SessionToken, true}, {a, "id", &s.AccountID, false}, {u, "email", &s.Email, false}, {m, "cookies", &s.Cookies, false}} {
		*p.dst, e = text(p.m, p.k, p.required)
		if e != nil {
			return model_Session{}, e
		}
	}
	return s, nil
}

// 仅返回位置与固定文案，不携带 JSON 原文或底层解析错误。
func model_decodeSession(raw string) (protocol_Map, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		position := len(raw)
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			position = int(syntax.Offset) - 1
		}
		return nil, model_sessionSyntaxError(raw, position)
	}
	end := int(decoder.InputOffset())
	rest := raw[end:]
	remaining := strings.TrimLeft(rest, " \r\n\t")
	if remaining != "" {
		return nil, model_sessionSyntaxError(raw, end+len(rest)-len(remaining))
	}
	m, ok := value.(protocol_Map)
	if !ok || m == nil {
		return nil, fmt.Errorf("Session JSON must be an object")
	}
	return m, nil
}

func model_sessionSyntaxError(raw string, position int) error {
	if position < 0 {
		position = 0
	}
	if position > len(raw) {
		position = len(raw)
	}
	prefix := raw[:position]
	line := strings.Count(prefix, "\n") + 1
	last := strings.LastIndex(prefix, "\n")
	column := utf8.RuneCountInString(prefix[last+1:]) + 1
	return fmt.Errorf("invalid Session JSON at line %d, column %d", line, column)
}
