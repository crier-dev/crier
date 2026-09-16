package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// decodeArgs strictly decodes a tool call's top-level arguments object.
//
// encoding/json silently DROPS members that no field of the destination claims,
// so a caller that misspells or invents an argument (`max_messges` for
// `max_messages`, `timeout_ms` for `timeout_s`) used to receive a SUCCESSFUL
// call whose parameter had been ignored and a default quietly substituted — the
// caller had no way to tell. Every tool handler decodes through this helper so
// that an unrecognized member becomes an ordinary in-band tool error naming it:
//
//	invalid arguments: unknown argument "max_messges"
//
// That is the same doctrine the HTTP side already applies (DF-CRIER-180): a
// parameter that cannot be honored is rejected, not silently accepted.
//
// Strictness deliberately stops at the TOP LEVEL. Nested values — `payload`,
// `body`, `capabilities`, and any opaque sub-object — reach the handler as
// map[string]any / json.RawMessage / any and are passed through untouched;
// their shape is the caller's business, so no nested decode may use
// DisallowUnknownFields. An absent or null arguments object means the empty
// object, which is what the no-argument tools (list_agents, mesh_peers)
// declare. The whole buffer must be consumed: trailing data after the object is
// itself an argument error.
func decodeArgs(args json.RawMessage, out any) error {
	raw := bytes.TrimSpace(args)
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}

	if unknown := unknownArguments(raw, out); len(unknown) > 0 {
		noun := "argument"
		if len(unknown) > 1 {
			noun = "arguments"
		}
		quoted := make([]string, len(unknown))
		for i, key := range unknown {
			quoted[i] = strconv.Quote(key)
		}
		return fmt.Errorf("invalid arguments: unknown %s %s", noun, strings.Join(quoted, ", "))
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	// Kept as a second line of defence behind unknownArguments: if that scan
	// ever fails to enumerate a member, the decode itself still rejects it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid arguments: unexpected trailing data after the arguments object")
	}
	return nil
}

// unknownArguments lists, in document order, the top-level members of raw that
// the type behind out does not declare. Go's own decoder is the arbiter: each
// member name is re-offered on its own (with a null value, which no field type
// rejects) to a fresh destination of the same type, so the matching rules —
// including json's case-insensitive field matching — are exactly the ones the
// real decode uses.
func unknownArguments(raw []byte, out any) []string {
	keys, err := topLevelKeys(raw)
	if err != nil {
		return nil
	}
	typ := reflect.TypeOf(out)
	if typ == nil || typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		return nil
	}
	typ = typ.Elem()

	var unknown []string
	for _, key := range keys {
		probe, err := json.Marshal(map[string]any{key: nil})
		if err != nil {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(probe))
		dec.DisallowUnknownFields()
		if err := dec.Decode(reflect.New(typ).Interface()); err != nil && isUnknownFieldError(err) {
			unknown = append(unknown, key)
		}
	}
	return unknown
}

// topLevelKeys returns the member names of a JSON object in the order they
// appear, without decoding their values. A non-object (or malformed) buffer
// returns an error and no keys; the real decode then reports the type error.
func topLevelKeys(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("arguments must be a JSON object")
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("arguments must be a JSON object")
		}
		keys = append(keys, key)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// isUnknownFieldError reports whether err is encoding/json's own
// DisallowUnknownFields rejection (its message is
// `json: unknown field "<name>"`).
func isUnknownFieldError(err error) bool {
	return strings.HasPrefix(err.Error(), "json: unknown field ")
}
