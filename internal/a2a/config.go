// Package a2a carries crier's OPT-IN A2A interoperability option
// (INT-A2A-001, specs/A2A-OPTION.md).
//
// A2A is an EXTRA in crier, not first-class support. The option is behind two
// halves of one gate: the server-side switch CR_A2A_ENABLED (default false,
// config.Config.A2AEnabled) and the per-agent `a2a` block modelled here. Either
// half alone is inert, and with the switch unset — the default posture —
// nothing in this package is reachable through any crier surface.
//
// This package holds the option's data shape and its strict decoder
// (INT-A2A-001), the Agent Card projection with the discovery constants
// (INT-A2A-002, card.go), the JSON-RPC 2.0 binding with the part mapping and the
// SSE contract (INT-A2A-003, rpc.go / parts.go / send.go) and the task-lifecycle
// mapping onto crier's inbox entry (INT-A2A-004, task.go). The remaining
// surfaces — the push-notification configs and the extended card — arrive with
// INT-A2A-005/006 and are enumerated in specs/A2A-OPTION.md §5.2.
package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Config is the optional per-agent `a2a` block on a registry row — the
// per-agent half of the A2A gate (specs/A2A-OPTION.md §4.2). It is serialized
// with omitempty, so an agent registered without the block is byte-identical
// to a pre-A2A registry row, and a block that is absent from the request stays
// absent from the response.
//
// The block is minimal on purpose: today it carries the opt-in flag only.
// Later rows extend it additively, through this same strict decoder.
type Config struct {
	// Enabled is the agent's opt-in to the A2A option. It is HALF the gate:
	// with CR_A2A_ENABLED off no A2A surface exists for any agent, and with
	// the agent's block absent or Enabled false no A2A surface targets this
	// agent. False — the zero value, so an empty `{}` block — means the agent
	// takes no part in A2A.
	Enabled bool `json:"enabled,omitempty"`
}

// OptedIn reports whether this block opts the agent in to A2A.
//
// It is the per-agent half of the gate, and it is fail-closed on purpose: an
// absent block (nil — every pre-A2A registration) and a present-but-false block
// ("a2a":{} or {"a2a":{"enabled":false}}) both mean the agent takes no part in
// A2A, so a surface that consults this can never advertise an agent that did not
// ask to be advertised (specs/A2A-OPTION.md §4.2).
func (c *Config) OptedIn() bool {
	return c != nil && c.Enabled
}

// DecodeConfig strictly decodes ONE `a2a` object, mirroring the discipline the
// `webhook` object is held to (internal/webhook.DecodeConfig, DF-CRIER-150):
// every member must be a field Config declares, and a member the type does not
// declare is an ERROR naming the offending key AND the keys that are accepted
// — never a silently dropped member.
//
// Why this exists: the HTTP registration paths decode the surrounding agent
// body permissively, so without this scan a misnamed member — {"enabld":true}
// instead of {"enabled":true} — would be dropped by encoding/json and the
// caller would silently get an agent that never opted in while believing it
// had. Strictness is scoped to THIS object: the top-level agent body and the
// neighbouring webhook/guard objects keep their own rules.
//
// A JSON null decodes to (nil, nil): the member was explicitly cleared, which
// the registration paths read as "no a2a block". A value that is neither an
// object nor null is left to the decoder's own type error (the scan cannot
// judge it, and the decode reports it).
func DecodeConfig(raw []byte) (*Config, error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		return nil, nil
	}

	if key, accepted, found := unknownMember(trimmed); found {
		return nil, fmt.Errorf("a2a: unknown field %q (accepted: %s)", key, strings.Join(accepted, ", "))
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	// Second line of defence behind unknownMember: if that scan ever fails to
	// enumerate a member, the decode itself still refuses it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("a2a: unexpected trailing data after the a2a object")
	}
	return &cfg, nil
}

// unknownMember finds the first member of the JSON object raw that Config does
// not declare. found=false when raw is not a JSON object this scan can judge
// (a non-object, a malformed buffer) — the decode then reports the error
// itself.
//
// The scan is driven by the type (declaresField re-offers each name to Go's own
// decoder, so json's case-insensitive field matching is the arbiter) rather
// than by a hand-written list of names, so it cannot drift from the struct it
// guards.
func unknownMember(raw []byte) (key string, accepted []string, found bool) {
	keys, ok := objectMemberKeys(raw)
	if !ok {
		return "", nil, false
	}
	typ := reflect.TypeOf(Config{})
	for _, k := range keys {
		if !declaresField(typ, k) {
			return k, acceptedKeys(typ), true
		}
	}
	return "", nil, false
}

// objectMemberKeys returns the member names of a JSON object in document
// order. ok=false when raw is not an object this scan can read.
func objectMemberKeys(raw []byte) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return nil, false
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isString := kt.(string)
		if !isString {
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
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

// acceptedKeys lists the JSON member names typ declares, in declaration order
// — the list a rejection prints so the caller can fix the typo without reading
// the source.
func acceptedKeys(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue // unexported
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
