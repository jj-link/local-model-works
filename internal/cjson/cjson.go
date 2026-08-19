// Package cjson renders arbitrary JSON values canonically: object keys
// sorted lexicographically, no insignificant whitespace, numbers in their
// shortest round-trip form. The output is stable input to digests (plan
// digests, run inputs) so byte-identity means semantic identity.
package cjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Marshal canonically encodes v.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var val any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	return encode(val)
}

func encode(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if t {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case json.Number:
		return []byte(t.String()), nil
	case string:
		return json.Marshal(t)
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, err := encode(e)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			b, err := encode(t[k])
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("cjson: unsupported type %T", v)
	}
}
