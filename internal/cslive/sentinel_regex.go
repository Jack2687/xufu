// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	fmt "fmt"
	regexp2 "github.com/dlclark/regexp2"
	strconv "strconv"
	strings "strings"
	time "time"
)

func (m *sentinel_machine) findScript(pattern string) (any, error) {
	if len(pattern) > m.limits.regexBytes {
		return nil, sentinel_resourceError("turnstile_regex_limit")
	}
	// 引擎允许可变长 lookbehind，但 Python 基线禁止；先保留原约束。
	if err := sentinel_validatePythonLookbehind(pattern); err != nil {
		return nil, sentinel_resourceError("turnstile_regex_invalid")
	}
	pattern = sentinel_normalizePythonRegex(pattern)
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, sentinel_resourceError("turnstile_regex_invalid")
	}
	document := m.window.get("document").(*sentinel_object)
	scripts, _ := document.get("scripts").(*sentinel_array)
	if scripts == nil {
		return nil, nil
	}
	for _, item := range scripts.values {
		if err = sentinel_checkDeadline(m.options.Deadline); err != nil {
			return nil, err
		}
		re.MatchTimeout = time.Until(m.options.Deadline)
		src := ""
		if script, ok := item.(*sentinel_object); ok {
			src = sentinel_strOrEmpty(script.get("src"))
		}
		matched, err := re.FindStringMatch(src)
		if err != nil {
			if e := sentinel_checkDeadline(m.options.Deadline); e != nil {
				return nil, e
			}
			return nil, sentinel_resourceError("turnstile_regex_timeout")
		}
		if err = sentinel_checkDeadline(m.options.Deadline); err != nil {
			return nil, err
		}
		if matched != nil {
			return matched.String(), nil
		}
	}
	return nil, nil
}

func sentinel_normalizePythonRegex(pattern string) string {
	var b strings.Builder
	characterClass := false
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			b.WriteByte('\\')
			i++
			if pattern[i] == 'Z' && !characterClass {
				b.WriteByte('z')
			} else {
				b.WriteByte(pattern[i])
			}
			continue
		}
		if pattern[i] == '[' {
			characterClass = true
		}
		if pattern[i] == ']' {
			characterClass = false
		}
		if !characterClass && strings.HasPrefix(pattern[i:], "(?P<") {
			b.WriteString("(?<")
			i += 3
			continue
		}
		if !characterClass && strings.HasPrefix(pattern[i:], "(?P=") {
			if end := strings.IndexByte(pattern[i+4:], ')'); end >= 0 {
				b.WriteString(`\k<`)
				b.WriteString(pattern[i+4 : i+4+end])
				b.WriteByte('>')
				i += 4 + end
				continue
			}
		}
		b.WriteByte(pattern[i])
	}
	return b.String()
}

type sentinel_regexWidth struct{ min, max int }
type sentinel_widthParser struct {
	text         []rune
	pos, capture int
	groups       map[string]sentinel_regexWidth
}

const sentinel_unboundedWidth = 1000000000

func sentinel_validatePythonLookbehind(pattern string) error {
	p := sentinel_widthParser{text: []rune(pattern), groups: map[string]sentinel_regexWidth{}}
	_, err := p.sequence()
	return err
}
func (p *sentinel_widthParser) sequence() (sentinel_regexWidth, error) {
	branch := sentinel_regexWidth{}
	var alternatives []sentinel_regexWidth
	for p.pos < len(p.text) && p.text[p.pos] != ')' {
		if p.text[p.pos] == '|' {
			alternatives = append(alternatives, branch)
			branch = sentinel_regexWidth{}
			p.pos++
			continue
		}
		atom, err := p.atom()
		if err != nil {
			return sentinel_regexWidth{}, err
		}
		if p.pos < len(p.text) {
			switch p.text[p.pos] {
			case '*':
				atom = sentinel_regexWidth{0, sentinel_unboundedWidth}
				p.pos++
			case '+':
				atom.max = sentinel_unboundedWidth
				p.pos++
			case '?':
				atom.min = 0
				p.pos++
			case '{':
				start := p.pos
				p.pos++
				from := p.pos
				for p.pos < len(p.text) && p.text[p.pos] >= '0' && p.text[p.pos] <= '9' {
					p.pos++
				}
				if from == p.pos {
					p.pos = start
					break
				}
				n, _ := strconv.Atoi(string(p.text[from:p.pos]))
				maxN := n
				if p.pos < len(p.text) && p.text[p.pos] == ',' {
					p.pos++
					from = p.pos
					for p.pos < len(p.text) && p.text[p.pos] >= '0' && p.text[p.pos] <= '9' {
						p.pos++
					}
					maxN = sentinel_unboundedWidth
					if from != p.pos {
						maxN, _ = strconv.Atoi(string(p.text[from:p.pos]))
					}
				}
				if p.pos < len(p.text) && p.text[p.pos] == '}' {
					p.pos++
					atom.min = min(sentinel_unboundedWidth, atom.min*n)
					atom.max = min(sentinel_unboundedWidth, atom.max*maxN)
				} else {
					p.pos = start
				}
			}
			if p.pos < len(p.text) && (p.text[p.pos] == '?' || p.text[p.pos] == '+') {
				p.pos++
			}
		}
		branch.min = min(sentinel_unboundedWidth, branch.min+atom.min)
		branch.max = min(sentinel_unboundedWidth, branch.max+atom.max)
	}
	alternatives = append(alternatives, branch)
	result := alternatives[0]
	for _, b := range alternatives[1:] {
		result.min = min(result.min, b.min)
		result.max = max(result.max, b.max)
	}
	return result, nil
}
func (p *sentinel_widthParser) atom() (sentinel_regexWidth, error) {
	r := p.text[p.pos]
	p.pos++
	switch r {
	case '^', '$':
		return sentinel_regexWidth{}, nil
	case '[':
		if p.pos < len(p.text) && p.text[p.pos] == '^' {
			p.pos++
		}
		if p.pos < len(p.text) && p.text[p.pos] == ']' {
			p.pos++
		}
		for p.pos < len(p.text) {
			c := p.text[p.pos]
			p.pos++
			if c == '\\' && p.pos < len(p.text) {
				p.pos++
				continue
			}
			if c == ']' {
				break
			}
		}
		return sentinel_regexWidth{1, 1}, nil
	case '\\':
		if p.pos >= len(p.text) {
			return sentinel_regexWidth{}, nil
		}
		c := p.text[p.pos]
		p.pos++
		if strings.ContainsRune("AbBZz", c) {
			return sentinel_regexWidth{}, nil
		}
		if c >= '1' && c <= '9' {
			key := string(c)
			for p.pos < len(p.text) && p.text[p.pos] >= '0' && p.text[p.pos] <= '9' {
				key += string(p.text[p.pos])
				p.pos++
			}
			if width, ok := p.groups[key]; ok {
				return width, nil
			}
			return sentinel_regexWidth{0, sentinel_unboundedWidth}, nil
		}
		extra := 0
		switch c {
		case 'x':
			extra = 2
		case 'u':
			extra = 4
		case 'U':
			extra = 8
		}
		p.pos = min(len(p.text), p.pos+extra)
		return sentinel_regexWidth{1, 1}, nil
	case '(':
		capture := true
		lookaround, lookbehind := false, false
		name := ""
		if p.pos < len(p.text) && p.text[p.pos] == '?' {
			capture = false
			p.pos++
			if p.pos >= len(p.text) {
				return sentinel_regexWidth{}, nil
			}
			switch p.text[p.pos] {
			case ':', '>':
				p.pos++
			case '=', '!':
				lookaround = true
				p.pos++
			case '<':
				if p.pos+1 < len(p.text) && (p.text[p.pos+1] == '=' || p.text[p.pos+1] == '!') {
					lookaround = true
					lookbehind = true
					p.pos += 2
				}
			case '#':
				for p.pos < len(p.text) && p.text[p.pos] != ')' {
					p.pos++
				}
				if p.pos < len(p.text) {
					p.pos++
				}
				return sentinel_regexWidth{}, nil
			case 'P':
				p.pos++
				if p.pos < len(p.text) && p.text[p.pos] == '=' {
					p.pos++
					start := p.pos
					for p.pos < len(p.text) && p.text[p.pos] != ')' {
						p.pos++
					}
					name = string(p.text[start:p.pos])
					if p.pos < len(p.text) {
						p.pos++
					}
					if width, ok := p.groups[name]; ok {
						return width, nil
					}
					return sentinel_regexWidth{0, sentinel_unboundedWidth}, nil
				}
				if p.pos < len(p.text) && p.text[p.pos] == '<' {
					capture = true
					p.pos++
					start := p.pos
					for p.pos < len(p.text) && p.text[p.pos] != '>' {
						p.pos++
					}
					name = string(p.text[start:p.pos])
					if p.pos < len(p.text) {
						p.pos++
					}
				}
			default:
				for p.pos < len(p.text) && strings.ContainsRune("aiLmsux-", p.text[p.pos]) {
					p.pos++
				}
				if p.pos < len(p.text) && p.text[p.pos] == ')' {
					p.pos++
					return sentinel_regexWidth{}, nil
				}
				if p.pos < len(p.text) && p.text[p.pos] == ':' {
					p.pos++
				}
			}
		}
		id := 0
		if capture {
			p.capture++
			id = p.capture
		}
		width, err := p.sequence()
		if err != nil {
			return sentinel_regexWidth{}, err
		}
		if p.pos < len(p.text) && p.text[p.pos] == ')' {
			p.pos++
		}
		if lookbehind && width.min != width.max {
			return sentinel_regexWidth{}, fmt.Errorf("look-behind requires fixed-width pattern")
		}
		if capture {
			p.groups[strconv.Itoa(id)] = width
			if name != "" {
				p.groups[name] = width
			}
		}
		if lookaround {
			return sentinel_regexWidth{}, nil
		}
		return width, nil
	}
	return sentinel_regexWidth{1, 1}, nil
}
