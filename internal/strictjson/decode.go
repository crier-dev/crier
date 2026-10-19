// Package strictjson decodes request structs without silently discarding fields.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Decode keeps encoding/json's type and case-insensitive field semantics while
// naming unknown fields with the accepted keys of the struct that guards them.
// Like Decoder.Decode, it consumes one JSON value. Maps and custom UnmarshalJSON
// implementations retain their own field semantics.
func Decode(r io.Reader, dst any) error {
	var raw json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return err
	}
	if err := checkFields(raw, reflect.TypeOf(dst)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

func checkFields(raw json.RawMessage, typ reflect.Type) error {
	if typ == nil || typ.Implements(unmarshalerType) {
		return nil
	}
	if typ.Kind() == reflect.Pointer {
		return checkFields(raw, typ.Elem())
	}
	if reflect.PointerTo(typ).Implements(unmarshalerType) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return nil // The real decoder reports type errors.
		}
		var accepted []string
		fields := make(map[string]reflect.Type)
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			accepted = append(accepted, name)
			fields[name] = f.Type
		}
		// Token order makes the first diagnostic deterministic, not map order.
		dec := json.NewDecoder(bytes.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return nil
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name := key.(string)
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				return err
			}
			field, ok := fields[name]
			if !ok {
				for _, candidate := range accepted {
					if strings.EqualFold(name, candidate) {
						field, ok = fields[candidate], true
						break
					}
				}
			}
			if !ok {
				return fmt.Errorf("unknown field %q (accepted: %s)", name, strings.Join(accepted, ", "))
			}
			if err := checkFields(value, field); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) == nil {
			for i, value := range values {
				if err := checkFields(value, typ.Elem()); err != nil {
					return fmt.Errorf("element %d: %w", i, err)
				}
			}
		}
	}
	return nil
}
