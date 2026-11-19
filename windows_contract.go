package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// These schemas are the immutable r2 handoff, not fetched from the network.
//
//go:embed contracts/media-windows-v1/schemas/*.json
var windowSchemas embed.FS

type windowRequest struct {
	Schema    string `json:"schema"`
	Version   int    `json:"version"`
	RequestID string `json:"request_id"`
	Operation string `json:"operation"`
	Source    struct {
		Provider       string  `json:"provider"`
		WatchID        string  `json:"watch_id"`
		PresentationID string  `json:"presentation_id"`
		Locale         *string `json:"presentation_locale"`
		Snapshot       *string `json:"expected_snapshot_id"`
	} `json:"source"`
	Video struct {
		Selection  string  `json:"selection"`
		ID         *string `json:"representation_id"`
		Substitute bool    `json:"allow_representation_substitution"`
	} `json:"video"`
	Windows  []windowSpec `json:"windows"`
	Delivery struct {
		Before string `json:"max_dependency_padding_before_seconds"`
		After  string `json:"max_dependency_padding_after_seconds"`
		Split  bool   `json:"allow_discontinuity_split"`
	} `json:"delivery"`
	Output struct {
		Directory string `json:"directory"`
	} `json:"output"`
}
type windowSpec struct {
	ID     string `json:"window_id"`
	Anchor string `json:"anchor"`
	Start  string `json:"start_seconds"`
	End    string `json:"end_seconds"`
	Before string `json:"context_before_seconds"`
	After  string `json:"context_after_seconds"`
	Policy string `json:"boundary_policy"`
}

type windowFailure string

func (e windowFailure) Error() string { return string(e) }
func wf(code string) error            { return windowFailure(code) }
func windowErrorCode(err error) string {
	var w windowFailure
	if errors.As(err, &w) {
		return string(w)
	}
	return "internal_error"
}

func rational(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || len(s) > 128 || r.Num().String()+"/"+r.Denom().String() != s {
		return nil, wf("invalid_request")
	}
	return r, nil
}
func rat(s string) *big.Rat         { r, _ := rational(s); return r }
func rs(r *big.Rat) string          { return r.Num().String() + "/" + r.Denom().String() }
func add(a, b *big.Rat) *big.Rat    { return new(big.Rat).Add(a, b) }
func sub(a, b *big.Rat) *big.Rat    { return new(big.Rat).Sub(a, b) }
func ticks(v, scale int64) *big.Rat { return new(big.Rat).SetFrac(big.NewInt(v), big.NewInt(scale)) }
func minRat(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) < 0 {
		return a
	}
	return b
}
func maxRat(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) > 0 {
		return a
	}
	return b
}

// strictJSON rejects duplicate keys (including escaped aliases), trailing data,
// invalid UTF-8 and excessive nesting before schema validation.
func strictJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, wf("invalid_request")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 32 {
			return nil, wf("invalid_request")
		}
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, e
					}
					key, ok := k.(string)
					if !ok {
						return nil, wf("invalid_request")
					}
					if _, ok = m[key]; ok {
						return nil, wf("invalid_request")
					}
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					m[key] = v
				}
				_, e = d.Token()
				return m, e
			case '[':
				a := []any{}
				for d.More() {
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				_, e = d.Token()
				return a, e
			default:
				return nil, wf("invalid_request")
			}
		}
		return t, nil
	}
	v, e := read(0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, wf("invalid_request")
	}
	return v, nil
}

// Validate only the vocabulary used by the bundled contract. This is not a
// general JSON Schema implementation; tests lock the supported keyword set.
func schemaValid(root, node map[string]any, v any) bool {
	if ref, ok := node["$ref"].(string); ok {
		var n any = root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := n.(map[string]any)
			if !ok {
				return false
			}
			n = m[part]
		}
		m, ok := n.(map[string]any)
		if !ok || !schemaValid(root, m, v) {
			return false
		}
	}
	equal := func(a, b any) bool {
		an, aok := a.(json.Number)
		bn, bok := b.(json.Number)
		if aok && bok {
			ar, ok := new(big.Rat).SetString(string(an))
			br, bok := new(big.Rat).SetString(string(bn))
			return ok && bok && ar.Cmp(br) == 0
		}
		return reflect.DeepEqual(a, b)
	}
	if c, ok := node["const"]; ok && !equal(c, v) {
		return false
	}
	if enum, ok := node["enum"].([]any); ok {
		found := false
		for _, c := range enum {
			found = found || equal(c, v)
		}
		if !found {
			return false
		}
	}
	if typ, ok := node["type"].(string); ok {
		valid := false
		switch typ {
		case "null":
			valid = v == nil
		case "object":
			_, valid = v.(map[string]any)
		case "array":
			_, valid = v.([]any)
		case "string":
			_, valid = v.(string)
		case "boolean":
			_, valid = v.(bool)
		case "integer":
			if n, ok := v.(json.Number); ok {
				r, ok := new(big.Rat).SetString(string(n))
				valid = ok && r.IsInt()
			}
		}
		if !valid {
			return false
		}
	}
	limit := func(key string, n int) bool {
		x, ok := node[key].(json.Number)
		if !ok {
			return true
		}
		k, _ := x.Int64()
		if strings.HasPrefix(key, "min") {
			return int64(n) >= k
		}
		return int64(n) <= k
	}
	if m, ok := v.(map[string]any); ok {
		if required, ok := node["required"].([]any); ok {
			for _, k := range required {
				if _, ok := m[k.(string)]; !ok {
					return false
				}
			}
		}
		props, _ := node["properties"].(map[string]any)
		for k, x := range m {
			if s, ok := props[k].(map[string]any); ok {
				if !schemaValid(root, s, x) {
					return false
				}
			} else if node["additionalProperties"] == false {
				return false
			}
		}
	}
	if a, ok := v.([]any); ok {
		if !limit("minItems", len(a)) || !limit("maxItems", len(a)) {
			return false
		}
		for i, x := range a {
			if s, ok := node["items"].(map[string]any); ok && !schemaValid(root, s, x) {
				return false
			}
			if node["uniqueItems"] == true {
				for _, y := range a[:i] {
					if equal(x, y) {
						return false
					}
				}
			}
		}
	}
	if s, ok := v.(string); ok {
		if !limit("minLength", utf8.RuneCountInString(s)) || !limit("maxLength", utf8.RuneCountInString(s)) {
			return false
		}
		if p, ok := node["pattern"].(string); ok {
			if strings.HasPrefix(p, "^(?!/") {
				if !safeWindowRelativePath(s) {
					return false
				}
			} else {
				p = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(p, `\u0000`, `\x{0000}`), `\u001f`, `\x{001f}`), `\u007f`, `\x{007f}`)
				r, e := regexp.Compile(p)
				if e != nil || !r.MatchString(s) {
					return false
				}
			}
		}
	}
	if n, ok := v.(json.Number); ok {
		r, ok := new(big.Rat).SetString(string(n))
		if !ok {
			return false
		}
		for _, key := range []string{"minimum", "maximum"} {
			if b, ok := node[key].(json.Number); ok {
				bound, _ := new(big.Rat).SetString(string(b))
				c := r.Cmp(bound)
				if (key == "minimum" && c < 0) || (key == "maximum" && c > 0) {
					return false
				}
			}
		}
	}
	for _, key := range []string{"oneOf", "allOf"} {
		if list, ok := node[key].([]any); ok {
			count := 0
			for _, s := range list {
				if schemaValid(root, s.(map[string]any), v) {
					count++
				}
			}
			if (key == "oneOf" && count != 1) || (key == "allOf" && count != len(list)) {
				return false
			}
		}
	}
	if cond, ok := node["if"].(map[string]any); ok {
		key := "else"
		if schemaValid(root, cond, v) {
			key = "then"
		}
		if s, ok := node[key].(map[string]any); ok && !schemaValid(root, s, v) {
			return false
		}
	}
	return true
}
func validateWindowJSON(name string, data []byte) error {
	b, e := windowSchemas.ReadFile("contracts/media-windows-v1/schemas/" + name + ".schema.json")
	if e != nil {
		return e
	}
	s, e := strictJSON(b)
	if e != nil {
		return e
	}
	v, e := strictJSON(data)
	if e != nil {
		return e
	}
	root := s.(map[string]any)
	if !schemaValid(root, root, v) {
		return wf("invalid_request")
	}
	return nil
}
func safeWindowRelativePath(s string) bool {
	if s == "" || strings.ContainsAny(s, "\\:") || strings.HasPrefix(s, "/") {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}
func parseWindowRequest(data []byte) (windowRequest, error) {
	var r windowRequest
	if len(data) > 1<<20 || validateWindowJSON("request", data) != nil {
		return r, wf("invalid_request")
	}
	if e := json.Unmarshal(data, &r); e != nil {
		return r, wf("invalid_request")
	}
	seen := map[string]bool{}
	for _, w := range r.Windows {
		if seen[w.ID] {
			return r, wf("invalid_request")
		}
		seen[w.ID] = true
		for _, s := range []string{w.Start, w.End, w.Before, w.After, r.Delivery.Before, r.Delivery.After} {
			if _, e := rational(s); e != nil {
				return r, e
			}
		}
		a, b := rat(w.Start), rat(w.End)
		if a.Cmp(b) >= 0 || (w.Anchor == "episode_start" && a.Sign() < 0) || (w.Anchor == "episode_end" && b.Sign() > 0) {
			return r, wf("invalid_request")
		}
	}
	return r, nil
}

func windowJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(fmt.Errorf("internal JSON encoding failure"))
	}
	return append(b, '\n')
}
