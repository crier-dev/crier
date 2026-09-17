package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// DecodeConfig strictly decodes ONE webhook object: every member must be a
// field Config declares, at every nesting level (batch, custom_schema,
// request_shape), and a member the type does not declare is an error naming
// the offending key AND the keys that are accepted (DF-CRIER-150).
//
// Why this exists: the HTTP registration paths decode the surrounding agent
// body permissively, so before this a misnamed member — {"mode":"blocking"}
// instead of {"delivery_mode":"blocking"} — was dropped by encoding/json and
// the caller silently got the async default while believing it had asked for
// blocking. Strictness is deliberately scoped to THIS object: the top-level
// agent body and the guard object stay permissive (a caller may carry extra
// keys there), and the MCP registration path is untouched.
//
// A JSON null decodes to (nil, nil): the member was explicitly cleared, which
// both registration paths read as "remove the webhook". A value that is
// neither an object nor null is left to the decoder's own type error.
func DecodeConfig(raw []byte) (*Config, error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		return nil, nil
	}

	if key, accepted, path, found := unknownMember(trimmed, reflect.TypeOf(Config{}), "webhook"); found {
		return nil, fmt.Errorf("%s: unknown field %q (accepted: %s)", path, key, strings.Join(accepted, ", "))
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	// Second line of defence behind unknownMember: if that scan ever fails to
	// enumerate a member, the decode itself still refuses it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("webhook: unexpected trailing data after the webhook object")
	}
	return &cfg, nil
}

// unknownMember finds the first member of the JSON object raw that the struct
// type typ does not declare, descending into nested config structs so a
// nested typo is named with its path (webhook.batch) instead of surfacing as a
// bare decoder error. found=false when raw is not an object this scan can
// judge (a non-object, a malformed buffer, or a nested value that is not an
// object) — the decode then reports the error itself.
func unknownMember(raw []byte, typ reflect.Type, path string) (key string, accepted []string, at string, found bool) {
	keys, values, ok := objectMembers(raw)
	if !ok {
		return "", nil, "", false
	}
	for i, k := range keys {
		if !declaresField(typ, k) {
			return k, acceptedKeys(typ), path, true
		}
		ft, ok := fieldType(typ, k)
		if !ok {
			continue
		}
		nt, ok := configStructType(ft)
		if !ok {
			continue
		}
		if kk, acc, pk, f := unknownMember(values[i], nt, path+"."+k); f {
			return kk, acc, pk, f
		}
	}
	return "", nil, "", false
}

// objectMembers returns the members of a JSON object in document order,
// preserving each member's raw value.
func objectMembers(raw []byte) (keys []string, values []json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return nil, nil, false
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, isString := kt.(string)
		if !isString {
			return nil, nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, nil, false
		}
		keys = append(keys, key)
		values = append(values, val)
	}
	return keys, values, true
}

// declaresField reports whether the struct type typ declares the JSON member
// name. Go's own decoder is the arbiter: the name is re-offered on its own
// (with a null value, which no field type rejects) to a fresh destination of
// the same type, so the matching rules — including json's case-insensitive
// field matching — are exactly the ones the real decode applies.
func declaresField(typ reflect.Type, name string) bool {
	probe, err := json.Marshal(map[string]any{name: nil})
	if err != nil {
		return true // cannot probe — assume declared; the decode reports it
	}
	dec := json.NewDecoder(bytes.NewReader(probe))
	dec.DisallowUnknownFields()
	return dec.Decode(reflect.New(typ).Interface()) == nil
}

// fieldType returns the Go type of the struct field the JSON member name maps
// to: exact tag match first, then Go's case-insensitive match.
func fieldType(typ reflect.Type, name string) (reflect.Type, bool) {
	var folded reflect.Type
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		if tag == name {
			return f.Type, true
		}
		if folded == nil && strings.EqualFold(tag, name) {
			folded = f.Type
		}
	}
	return folded, folded != nil
}

// acceptedKeys lists the JSON member names typ declares, in declaration order
// — the list a rejection prints so the caller can fix the typo without
// reading the source.
func acceptedKeys(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// configStructType dereferences pointers and reports whether t is a struct
// whose own members this scan must enforce. A type with its own
// json.Unmarshaler decides its own members (json.RawMessage, time.Time,
// maps, slices, scalars and unknown structs are all skipped) — those are the
// decoder's business, not the scan's.
func configStructType(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, false
	}
	if reflect.PointerTo(t).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil, false
	}
	return t, true
}
