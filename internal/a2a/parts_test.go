package a2a

// parts_test.go — INT-A2A-003: the part mapping, both directions.
//
// The row's acceptance criterion for this half is "a round-trip test proves a
// multi-part A2A message survives to a crier consumer with alt/tags intact", so
// the central case here is the STRONGER form of it: an A2A message taken from
// the wire, projected onto the crier payload a consumer reads, and projected
// back — byte-for-byte the same JSON. The refusals are the other half: a part
// whose oneof is not exactly one member, and a metadata member of the wrong
// type, are refused with the field path named rather than coerced.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// twoPartMessageJSON is the wire form of the 2-part message the row's evidence
// asks for: a text part carrying alt/tags/caption in its metadata, and a url
// file part. It is written as the JSON a client would send, so the test starts
// from the specification's shape rather than from these Go types.
const twoPartMessageJSON = `{
  "messageId": "msg-8f14e45f",
  "contextId": "ctx-1",
  "role": "ROLE_USER",
  "parts": [
    {
      "text": "deploy the canary",
      "mediaType": "text/plain",
      "metadata": {"alt": "operator instruction", "tags": ["deploy", "canary"], "caption": "step 1"}
    },
    {
      "url": "https://assets.example.com/canary.png",
      "filename": "canary.png",
      "mediaType": "image/png",
      "metadata": {"alt": "the canary image", "run": 7}
    }
  ],
  "metadata": {"trace": "abc123"},
  "extensions": ["https://example.com/extensions/citations/v1"],
  "referenceTaskIds": ["task-1"]
}`

// TestParts_TwoPartMessageSurvivesToACrierConsumer is the row's evidence test at
// the mapping layer: what the consumer reads off the delivery payload.
func TestParts_TwoPartMessageSurvivesToACrierConsumer(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(twoPartMessageJSON), &msg); err != nil {
		t.Fatalf("decode the A2A message: %v", err)
	}

	env, err := EnvelopeFromMessage(&msg)
	if err != nil {
		t.Fatalf("project the message onto a crier payload: %v", err)
	}
	if len(env.Parts) != 2 {
		t.Fatalf("projected parts = %d, want 2", len(env.Parts))
	}

	// The consumer's view: a text part whose alt/tags/caption are first-class,
	// and a file part that is a REFERENCE (uri), never a fetched body.
	text := env.Parts[0]
	if text.Type != PartTypeText || text.Text != "deploy the canary" {
		t.Errorf("part 0 = %+v, want a text part carrying the text", text)
	}
	if text.Alt != "operator instruction" {
		t.Errorf("part 0 alt = %q, want the metadata alt", text.Alt)
	}
	if !reflect.DeepEqual(text.Tags, []string{"deploy", "canary"}) {
		t.Errorf("part 0 tags = %v, want [deploy canary]", text.Tags)
	}
	if text.Caption != "step 1" {
		t.Errorf("part 0 caption = %q, want the metadata caption", text.Caption)
	}
	if text.MediaType != "text/plain" {
		t.Errorf("part 0 media_type = %q, want text/plain", text.MediaType)
	}

	file := env.Parts[1]
	if file.Type != PartTypeFile || file.File == nil || file.File.URI != "https://assets.example.com/canary.png" {
		t.Errorf("part 1 = %+v, want a file part referencing the url", file)
	}
	if file.Filename != "canary.png" {
		t.Errorf("part 1 filename = %q, want canary.png", file.Filename)
	}
	if file.Alt != "the canary image" {
		t.Errorf("part 1 alt = %q, want the metadata alt", file.Alt)
	}
	// Metadata the lift does not own rides through untouched.
	if got := file.Metadata["run"]; got != float64(7) {
		t.Errorf("part 1 metadata[run] = %v (%T), want the untouched 7", got, got)
	}

	// The message-level facts crier has no field for.
	if env.MessageID != "msg-8f14e45f" || env.ContextID != "ctx-1" || env.Role != RoleUser {
		t.Errorf("envelope ids/role = %q/%q/%q", env.MessageID, env.ContextID, env.Role)
	}
	if len(env.Extensions) != 1 || len(env.ReferenceTaskIDs) != 1 {
		t.Errorf("extensions/reference_task_ids = %v/%v, want both preserved", env.Extensions, env.ReferenceTaskIDs)
	}
}

// TestParts_EnvelopeRoundTripsBackToTheSameA2AMessage proves the mapping is
// faithful in both directions: the JSON a client sent is the JSON it would get
// back. Anything lost in the projection shows up here.
func TestParts_EnvelopeRoundTripsBackToTheSameA2AMessage(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(twoPartMessageJSON), &msg); err != nil {
		t.Fatalf("decode the A2A message: %v", err)
	}
	env, err := EnvelopeFromMessage(&msg)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	back, err := env.Message()
	if err != nil {
		t.Fatalf("project back: %v", err)
	}

	original := decodeAny(t, []byte(twoPartMessageJSON))
	rebuilt := marshalAny(t, back)
	if !reflect.DeepEqual(original, rebuilt) {
		t.Errorf("the A2A message did not survive the round trip:\noriginal = %#v\nrebuilt  = %#v", original, rebuilt)
	}
}

// TestParts_InlineBytesBecomeAVerifiableReference covers the `raw` file part: the
// bytes stay the base64 they arrived as, and the part gains the size and digest
// of what those bytes decode to.
func TestParts_InlineBytesBecomeAVerifiableReference(t *testing.T) {
	payload := []byte("canary")
	raw := base64.StdEncoding.EncodeToString(payload)
	msg, err := json.Marshal(map[string]any{
		"messageId": "m1", "role": "ROLE_USER",
		"parts": []any{map[string]any{"raw": raw, "mediaType": "application/octet-stream"}},
	})
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	var decoded Message
	if err := json.Unmarshal(msg, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	env, err := EnvelopeFromMessage(&decoded)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	file := env.Parts[0].File
	if file == nil || file.Bytes != raw {
		t.Fatalf("file part = %+v, want the base64 bytes carried verbatim", file)
	}
	if file.Size != len(payload) {
		t.Errorf("size = %d, want %d (the DECODED length)", file.Size, len(payload))
	}
	sum := sha256.Sum256(payload)
	if want := "sha256:" + hex.EncodeToString(sum[:]); file.Hash != want {
		t.Errorf("hash = %q, want %q — the digest of the DECODED bytes", file.Hash, want)
	}
	if file.URI != "" {
		t.Errorf("uri = %q, want empty for an inline part", file.URI)
	}

	// And back: the inline bytes are what the A2A part carries again.
	back, err := env.Message()
	if err != nil {
		t.Fatalf("project back: %v", err)
	}
	if back.Parts[0].Raw == nil || *back.Parts[0].Raw != raw {
		t.Errorf("rebuilt part = %+v, want the raw base64 back", back.Parts[0])
	}
	if back.Parts[0].Data != nil || back.Parts[0].URL != nil || back.Parts[0].Text != nil {
		t.Errorf("rebuilt part carries more than one oneof member: %+v", back.Parts[0])
	}
}

// TestParts_RefusesAPartWhoseOneofIsNotExactlyOne pins the strictness: the
// content oneof is enforced, not inferred, and the refusal names the path.
func TestParts_RefusesAPartWhoseOneofIsNotExactlyOne(t *testing.T) {
	cases := []struct {
		name      string
		parts     string
		wantField string
		wantIn    string
	}{
		{
			name:      "no content member",
			parts:     `[{"metadata":{"alt":"x"}}]`,
			wantField: "message.parts[0]",
			wantIn:    "exactly one of text, raw, url or data is required",
		},
		{
			name:      "the 0.3.0 kind-discriminated shape",
			parts:     `[{"kind":"text","text":"hello"}]`,
			wantField: "message.parts",
			wantIn:    `unknown field "kind"`,
		},
		{
			name:      "two content members",
			parts:     `[{"text":"a","data":{"b":1}}]`,
			wantField: "message.parts[0]",
			wantIn:    "exactly one of text, raw, url or data is allowed",
		},
		{
			name:      "inline bytes that are not base64",
			parts:     `[{"raw":"not base64!!"}]`,
			wantField: "message.parts[0].raw",
			wantIn:    "not valid base64",
		},
		{
			name:      "tags that are not a list",
			parts:     `[{"text":"a","metadata":{"tags":"deploy"}}]`,
			wantField: "message.parts[0].metadata.tags",
			wantIn:    "must be an array of strings",
		},
		{
			name:      "a tag that is not a string",
			parts:     `[{"text":"a","metadata":{"tags":["ok",3]}}]`,
			wantField: "message.parts[0].metadata.tags[1]",
			wantIn:    "must be a string",
		},
		{
			name:      "an alt that is an object",
			parts:     `[{"text":"a","metadata":{"alt":{"text":"no"}}}]`,
			wantField: "message.parts[0].metadata.alt",
			wantIn:    "must be a string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, rpcErr := DecodeSendMessageParams([]byte(`{"tenant":"agent-b","message":{"messageId":"m1","role":"ROLE_USER","parts":` + tc.parts + `}}`))
			if rpcErr != nil {
				// A refusal at decode time is fine as long as it names the field.
				if !strings.Contains(rpcErr.Message, tc.wantField) || !strings.Contains(rpcErr.Message, tc.wantIn) {
					t.Fatalf("refused with %q, want it to name %q and %q", rpcErr.Message, tc.wantField, tc.wantIn)
				}
				return
			}
			_, err := EnvelopeFromMessage(params.Message)
			if err == nil {
				t.Fatal("the part was accepted; want a refusal")
			}
			var invalid *InvalidParamsError
			if !asInvalidParams(err, &invalid) {
				t.Fatalf("refusal = %v (%T), want an *InvalidParamsError", err, err)
			}
			if invalid.Field != tc.wantField {
				t.Errorf("refusal field = %q, want %q", invalid.Field, tc.wantField)
			}
			if !strings.Contains(invalid.Detail, tc.wantIn) {
				t.Errorf("refusal detail = %q, want it to contain %q", invalid.Detail, tc.wantIn)
			}
		})
	}
}

// TestPartsFromPayload covers the payload→parts projection used for a pushed
// reply and for a relay event: this binding's envelopes contribute their parts,
// and anything else is carried as one data part, verbatim.
func TestPartsFromPayload(t *testing.T) {
	envelope := json.RawMessage(`{"parts":[{"type":"text","text":"hi","alt":"greeting","tags":["t"]}]}`)
	parts, err := PartsFromPayload(envelope)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if len(parts) != 1 || parts[0].Text == nil || *parts[0].Text != "hi" {
		t.Fatalf("envelope parts = %+v, want the projected text part", parts)
	}
	if alt, ok := parts[0].Metadata["alt"].(string); !ok || alt != "greeting" {
		t.Errorf("metadata = %v, want the lifted alt back in metadata", parts[0].Metadata)
	}

	opaque := json.RawMessage(`{"anything":["at","all"]}`)
	parts, err = PartsFromPayload(opaque)
	if err != nil {
		t.Fatalf("opaque: %v", err)
	}
	if len(parts) != 1 || string(parts[0].Data) != string(opaque) {
		t.Fatalf("opaque parts = %+v, want the payload verbatim as one data part", parts)
	}
}

// TestTaskTopic pins the relay topic convention: a task id that can be a topic
// segment yields the namespaced topic, and one that cannot yields nothing rather
// than a mangled name.
func TestTaskTopic(t *testing.T) {
	if got, ok := TaskTopic("8f14e45fceea167a5a36dedd"); !ok || got != "a2a.task.8f14e45fceea167a5a36dedd" {
		t.Errorf("TaskTopic = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "has space", "has/slash", "dot.ted", strings.Repeat("x", 201)} {
		if got, ok := TaskTopic(bad); ok {
			t.Errorf("TaskTopic(%q) = %q, want no topic", bad, got)
		}
	}
}

// TestParseEnvelope distinguishes this binding's payload from an opaque one.
func TestParseEnvelope(t *testing.T) {
	if _, ok := ParseEnvelope(json.RawMessage(`{"parts":[{"type":"text","text":""}]}`)); !ok {
		t.Error("an envelope with a parts array was not recognised")
	}
	for _, notAnEnvelope := range []string{`{"note":"no parts"}`, `[1,2]`, `"text"`, `null`, `{}`} {
		if _, ok := ParseEnvelope(json.RawMessage(notAnEnvelope)); ok {
			t.Errorf("%s was recognised as an envelope", notAnEnvelope)
		}
	}
}

// asInvalidParams is errors.As for *InvalidParamsError, kept local so this file
// reads without an errors import at every call site.
func asInvalidParams(err error, target **InvalidParamsError) bool {
	ip, ok := err.(*InvalidParamsError)
	if ok {
		*target = ip
	}
	return ok
}

// decodeAny decodes JSON into the generic shape a comparison wants.
func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// marshalAny renders a value back to the generic decoded shape, so a comparison
// is about the JSON, not about Go's types.
func marshalAny(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return decodeAny(t, raw)
}
