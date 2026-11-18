// json.go: a small JSON parser and writer, hand-rolled (no encoding/json:
// smaller binary, no reflection, and full control over output bytes).
//
// The parser produces plain Go values: map[string]any, []any, string,
// float64, bool, nil. The writer is a byte builder with typed append
// helpers; strings are escaped per RFC 8259, non-ASCII emitted raw UTF-8.
package main

import (
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ParseJSON parses b into a Go value. It returns an error for any
// malformed input, trailing bytes included.
func ParseJSON(b []byte) (any, error) {
	p := &jparser{b: b}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, errJSON
	}
	return v, nil
}

type jsonError string

func (e jsonError) Error() string { return string(e) }

const errJSON = jsonError("invalid JSON")

type jparser struct {
	b []byte
	i int
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
			continue
		}
		return
	}
}

func (p *jparser) value() (any, error) {
	p.ws()
	if p.i >= len(p.b) {
		return nil, errJSON
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, err := p.str()
		return s, err
	case c == 't':
		if p.lit("true") {
			return true, nil
		}
	case c == 'f':
		if p.lit("false") {
			return false, nil
		}
	case c == 'n':
		if p.lit("null") {
			return nil, nil
		}
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, errJSON
}

func (p *jparser) lit(s string) bool {
	if p.i+len(s) > len(p.b) || string(p.b[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

func (p *jparser) number() (any, error) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) {
		c := p.b[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
			continue
		}
		break
	}
	if start == p.i {
		return nil, errJSON
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return nil, errJSON
	}
	return f, nil
}

func (p *jparser) str() (string, error) {
	// p.b[p.i] == '"'
	p.i++
	start := p.i
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			if out == nil {
				return string(p.b[start : p.i-1]), nil
			}
			out = append(out, p.b[start:p.i-1]...)
			return string(out), nil
		}
		if c == '\\' {
			if out == nil {
				out = make([]byte, 0, 32)
			}
			out = append(out, p.b[start:p.i]...)
			p.i++
			if p.i >= len(p.b) {
				return "", errJSON
			}
			esc := p.b[p.i]
			p.i++
			switch esc {
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			case '/':
				out = append(out, '/')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				r, err := p.unicodeEscape()
				if err != nil {
					return "", err
				}
				out = utf8.AppendRune(out, r)
			default:
				return "", errJSON
			}
			start = p.i
			continue
		}
		if c < 0x20 {
			return "", errJSON
		}
		p.i++
	}
	return "", errJSON
}

func (p *jparser) unicodeEscape() (rune, error) {
	if p.i+4 > len(p.b) {
		return 0, errJSON
	}
	v, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, errJSON
	}
	p.i += 4
	r := rune(v)
	if utf16.IsSurrogate(r) {
		// Expect a \uXXXX low surrogate next.
		if p.i+6 <= len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
			v2, err := strconv.ParseUint(string(p.b[p.i+2:p.i+6]), 16, 32)
			if err == nil {
				lo := rune(v2)
				if dec := utf16.DecodeRune(r, lo); dec != utf8.RuneError {
					p.i += 6
					return dec, nil
				}
			}
		}
		return utf8.RuneError, nil
	}
	return r, nil
}

func (p *jparser) object() (any, error) {
	p.i++ // {
	m := make(map[string]any)
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return m, nil
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, errJSON
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, errJSON
		}
		p.i++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		m[k] = v
		p.ws()
		if p.i >= len(p.b) {
			return nil, errJSON
		}
		if p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.b[p.i] == '}' {
			p.i++
			return m, nil
		}
		return nil, errJSON
	}
}

func (p *jparser) array() (any, error) {
	p.i++ // [
	a := []any{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return a, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		p.ws()
		if p.i >= len(p.b) {
			return nil, errJSON
		}
		if p.b[p.i] == ',' {
			p.i++
			continue
		}
		if p.b[p.i] == ']' {
			p.i++
			return a, nil
		}
		return nil, errJSON
	}
}

// --- field access helpers for parsed request bodies ---

func jobj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func jstr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func jstrOK(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	s, ok := m[key].(string)
	return s, ok
}

func jint(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func jbool(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	if b, ok := m[key].(bool); ok {
		return b
	}
	return def
}

func jarr(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	if a, ok := m[key].([]any); ok {
		return a
	}
	return nil
}

func jstrs(m map[string]any, key string) []string {
	a := jarr(m, key)
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- JSON writer ---

// jb is a JSON byte builder. It writes objects and arrays with correct
// comma placement; field values are appended with typed helpers.
// key() marks that a value follows (afterKey), so the value writer
// does not emit a separator comma; every value path clears the mark.
type jb struct {
	b        []byte
	stack    []byte // 'o' object, 'a' array
	needsC   []bool // per level: next item needs a comma
	afterKey bool   // a key was just written; the next value takes no comma
}

func newJB(capacity int) *jb {
	return &jb{b: make([]byte, 0, capacity)}
}

func (w *jb) comma() {
	if w.afterKey {
		w.afterKey = false
		return
	}
	n := len(w.needsC)
	if n == 0 {
		return
	}
	if w.needsC[n-1] {
		w.b = append(w.b, ',')
	} else {
		w.needsC[n-1] = true
	}
}

func (w *jb) obj() *jb {
	w.comma()
	w.b = append(w.b, '{')
	w.stack = append(w.stack, 'o')
	w.needsC = append(w.needsC, false)
	return w
}

func (w *jb) arr() *jb {
	w.comma()
	w.b = append(w.b, '[')
	w.stack = append(w.stack, 'a')
	w.needsC = append(w.needsC, false)
	return w
}

// key starts an object field; the value follows with a val* call or obj()/arr().
func (w *jb) key(k string) *jb {
	w.comma()
	w.b = appendJSONStr(w.b, k)
	w.b = append(w.b, ':')
	w.afterKey = true
	return w
}

// val appends a pre-encoded value right after a key (no comma).
func (w *jb) val(v []byte) *jb {
	w.afterKey = false
	w.b = append(w.b, v...)
	return w
}

// valStr appends a string value right after a key (no comma).
func (w *jb) valStr(s string) *jb {
	w.afterKey = false
	w.b = appendJSONStr(w.b, s)
	return w
}

func (w *jb) end() *jb {
	n := len(w.stack)
	if n == 0 {
		return w
	}
	if w.stack[n-1] == 'o' {
		w.b = append(w.b, '}')
	} else {
		w.b = append(w.b, ']')
	}
	w.stack = w.stack[:n-1]
	w.needsC = w.needsC[:n-1]
	return w
}

func (w *jb) str(s string) *jb {
	w.comma()
	w.b = appendJSONStr(w.b, s)
	return w
}

func (w *jb) strField(k, s string) *jb {
	w.key(k)
	w.afterKey = false
	w.b = appendJSONStr(w.b, s)
	return w
}

func (w *jb) int(v int) *jb {
	w.comma()
	w.b = strconv.AppendInt(w.b, int64(v), 10)
	return w
}

func (w *jb) intField(k string, v int) *jb {
	w.key(k)
	w.afterKey = false
	w.b = strconv.AppendInt(w.b, int64(v), 10)
	return w
}

func (w *jb) int64Field(k string, v int64) *jb {
	w.key(k)
	w.afterKey = false
	w.b = strconv.AppendInt(w.b, v, 10)
	return w
}

func (w *jb) bool(v bool) *jb {
	w.comma()
	if v {
		w.b = append(w.b, "true"...)
	} else {
		w.b = append(w.b, "false"...)
	}
	return w
}

func (w *jb) boolField(k string, v bool) *jb {
	w.key(k)
	w.afterKey = false
	if v {
		w.b = append(w.b, "true"...)
	} else {
		w.b = append(w.b, "false"...)
	}
	return w
}

func (w *jb) null() *jb {
	w.comma()
	w.b = append(w.b, "null"...)
	return w
}

func (w *jb) nullField(k string) *jb {
	w.key(k)
	w.afterKey = false
	w.b = append(w.b, "null"...)
	return w
}

// raw appends pre-encoded JSON (a value) without escaping.
func (w *jb) raw(v []byte) *jb {
	w.comma()
	w.b = append(w.b, v...)
	return w
}

func (w *jb) rawField(k string, v []byte) *jb {
	w.key(k)
	w.afterKey = false
	w.b = append(w.b, v...)
	return w
}

func (w *jb) bytes() []byte { return w.b }

// appendJSONStr appends s as a JSON string literal (with quotes).
func appendJSONStr(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			if c < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0')
				dst = append(dst, hexDigit(c>>4), hexDigit(c&0xf))
			} else {
				dst = append(dst, c)
			}
		}
	}
	dst = append(dst, '"')
	return dst
}

const hexDigits = "0123456789abcdef"

func hexDigit(v byte) byte { return hexDigits[v&0xf] }
