// parts.go — the A2A data model crier speaks and the PART MAPPING in both
// directions (INT-A2A-003, specs/A2A-OPTION.md §3 and §5.4).
//
// The interesting half of this binding is the part model. A2A's Part (§4.1.6)
// carries a oneof of content — text, raw bytes, a url, or structured data —
// plus a free-form metadata object, a filename and a media type. crier's
// delivery payload is opaque JSON, so the projection is lossless by
// construction: an A2A message becomes a crier payload whose `parts` each carry
// their content AND the alt/tags/caption the row's mapping names, and the
// inverse rebuilds the very same A2A parts.
//
// Faithfulness rules, stated so no later reader has to infer them:
//
//   - the ONEOF is enforced, not inferred: exactly one of text / raw / url /
//     data per part, in BOTH directions. A part with two content fields, or
//     none, is refused — never guessed at.
//   - Part.metadata is preserved VERBATIM, and its `alt`, `tags` and `caption`
//     members are additionally lifted onto the crier part's own fields, which
//     is what makes them addressable by a crier consumer that knows nothing
//     about A2A. Lifting a wrongly-typed `alt` (an object) is refused rather
//     than coerced.
//   - a file part is reference-or-inline: `url` stays a reference crier never
//     dereferences, `raw` is kept as the base64 it arrived as and gains the
//     size and SHA-256 of the DECODED bytes, so a consumer can verify what it
//     was sent without this server ever holding a file handle.
//   - a data part is the existing opaque payload, untouched.
//
// The named deviation from the row's parenthetical — "role + alt + tags +
// reference-or-inline" — is `role`: A2A's Role is MESSAGE-scoped (§4.1.5), so
// it is carried once, on the crier envelope, rather than duplicated onto every
// part where the two copies could disagree. That is the same "no second source
// that could disagree" rule the whole option is held to.
package a2a

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// A2A message roles (§4.1.5).
const (
	RoleUnspecified = "ROLE_UNSPECIFIED"
	RoleUser        = "ROLE_USER"
	RoleAgent       = "ROLE_AGENT"
)

// crier part types — the `type` discriminator of a projected part.
const (
	PartTypeText = "text"
	PartTypeFile = "file"
	PartTypeData = "data"
)

// DefaultMediaType is what a part without a media type is reported as when a
// media type is required (a crier text part projected back onto an A2A part).
// It is the media type crier's own delivery wire uses.
const DefaultMediaType = "application/json"

// hashPrefix names the digest algorithm next to the digest, so the field is
// self-describing and a later algorithm cannot be mistaken for this one.
const hashPrefix = "sha256:"

// Part is one section of an A2A Message or Artifact (§4.1.6), in the JSON
// shape A2A v1.0.0 serializes (camelCase per §5.5).
//
// The oneof members are POINTERS on purpose: proto3 oneof fields have explicit
// presence, so `{"text":""}` is a text part with empty content and a part with
// no content member at all is a different (invalid) thing. Plain Go strings
// could not tell those apart, and this binding refuses rather than guesses.
type Part struct {
	Text      *string         `json:"text,omitempty"`
	Raw       *string         `json:"raw,omitempty"`
	URL       *string         `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

// TextPart returns a text part carried as s (which may be empty: the oneof
// member is present).
func TextPart(s string) Part { return Part{Text: &s} }

// URLPart returns a file part referencing url.
func URLPart(url string) Part { return Part{URL: &url} }

// DataPart returns a structured-data part carrying v serialized as JSON.
func DataPart(v any) (Part, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Part{}, err
	}
	return Part{Data: raw}, nil
}

// contentMembers counts the oneof members this part carries.
func (p Part) contentMembers() []string {
	var set []string
	if p.Text != nil {
		set = append(set, "text")
	}
	if p.Raw != nil {
		set = append(set, "raw")
	}
	if p.URL != nil {
		set = append(set, "url")
	}
	if len(bytes.TrimSpace(p.Data)) > 0 {
		set = append(set, "data")
	}
	return set
}

// Message is one unit of A2A communication (§4.1.4).
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             string         `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// TaskState is the A2A task lifecycle (§4.1.3), serialized as the protoJSON
// enum NAME (§5.5).
type TaskState string

// The task states this binding can assert, verbatim from §4.1.3.
const (
	TaskStateSubmitted   TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking     TaskState = "TASK_STATE_WORKING"
	TaskStateCompleted   TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed      TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled    TaskState = "TASK_STATE_CANCELED"
	TaskStateRejected    TaskState = "TASK_STATE_REJECTED"
	TaskStateUnspecified TaskState = "TASK_STATE_UNSPECIFIED"
)

// IsTerminal reports whether a task state ends the task (§3.1.2: the stream
// MUST close when the task reaches one of these).
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	default:
		return false
	}
}

// TaskStatus is the state plus the optional message and timestamp (§4.1.2).
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

// Task is the core unit of action (§4.1.1). In crier the id is the delivery's
// own message id (the inbox entry, or the pushed delivery) — there is no second
// task store to disagree with it.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Artifact is a task output (§4.1.7).
type Artifact struct {
	ArtifactID string         `json:"artifactId"`
	Name       string         `json:"name,omitempty"`
	Parts      []Part         `json:"parts"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// TaskStatusUpdateEvent is one status change on a stream (§4.2.1).
type TaskStatusUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskArtifactUpdateEvent is one artifact delta on a stream (§4.2.2).
type TaskArtifactUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Artifact  Artifact       `json:"artifact"`
	Append    bool           `json:"append,omitempty"`
	LastChunk bool           `json:"lastChunk,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// StreamResponse wraps exactly one of the four streaming payloads (§3.2.3).
// The oneof is enforced by construction: every setter below clears the others.
type StreamResponse struct {
	Task           *Task                    `json:"task,omitempty"`
	Message        *Message                 `json:"message,omitempty"`
	StatusUpdate   *TaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

// StreamTask wraps a task as a stream response.
func StreamTask(t *Task) StreamResponse { return StreamResponse{Task: t} }

// StreamMessage wraps a direct message as a stream response.
func StreamMessage(m *Message) StreamResponse { return StreamResponse{Message: m} }

// StreamStatus wraps a status update as a stream response.
func StreamStatus(e *TaskStatusUpdateEvent) StreamResponse { return StreamResponse{StatusUpdate: e} }

// SendMessageResponse is the `result` of SendMessage (§9.4.1): exactly one of a
// Task (the work is being processed, track it) or a direct Message (a simple
// interaction answered in one step). The oneof is enforced by construction.
type SendMessageResponse struct {
	Task    *Task    `json:"task,omitempty"`
	Message *Message `json:"message,omitempty"`
}

// StreamArtifact wraps an artifact update as a stream response.
func StreamArtifact(e *TaskArtifactUpdateEvent) StreamResponse {
	return StreamResponse{ArtifactUpdate: e}
}

// FileRef is the crier side of a file part: reference-or-inline. It is exactly
// one of uri (a reference crier stores and never dereferences) or bytes (the
// inline content, as the base64 it arrived as) plus what a consumer needs to
// verify it.
type FileRef struct {
	URI   string `json:"uri,omitempty"`
	Bytes string `json:"bytes,omitempty"`
	Size  int    `json:"size,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

// MessagePart is one crier message part — the projection of an A2A Part onto
// the delivery payload's own shape. A crier consumer reads `alt`/`tags`/
// `caption` and the reference-or-inline `file` without knowing A2A exists.
type MessagePart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	File      *FileRef        `json:"file,omitempty"`
	Alt       string          `json:"alt,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Caption   string          `json:"caption,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// Envelope is the crier delivery payload an A2A message becomes: the parts,
// plus the message-level facts (role, ids, extensions, metadata) crier does not
// have a field for. Every member is optional except `parts`, so a payload with
// no A2A provenance behind it is never mistaken for one of these.
type Envelope struct {
	Parts            []MessagePart  `json:"parts"`
	MessageID        string         `json:"message_id,omitempty"`
	ContextID        string         `json:"context_id,omitempty"`
	TaskID           string         `json:"task_id,omitempty"`
	Role             string         `json:"role,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"reference_task_ids,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

// reserved metadata keys: the A2A Part.metadata members this binding lifts onto
// the crier part's own fields. They are preserved verbatim in the part's
// `metadata` as well, so nothing rides on the lift being reversible.
const (
	metaAlt     = "alt"
	metaTags    = "tags"
	metaCaption = "caption"
)

// EnvelopeFromParts assembles the crier payload from an A2A message's parts and
// message-level facts. The parts are projected by partFromA2A, which is where
// the oneof and the metadata typing are enforced.
func EnvelopeFromMessage(m *Message) (*Envelope, error) {
	if m == nil {
		return nil, invalidParams("message", "is required")
	}
	parts := make([]MessagePart, 0, len(m.Parts))
	for i, p := range m.Parts {
		mp, err := partFromA2A(p, fmt.Sprintf("message.parts[%d]", i))
		if err != nil {
			return nil, err
		}
		parts = append(parts, mp)
	}
	return &Envelope{
		Parts:            parts,
		MessageID:        m.MessageID,
		ContextID:        m.ContextID,
		TaskID:           m.TaskID,
		Role:             m.Role,
		Extensions:       m.Extensions,
		ReferenceTaskIDs: m.ReferenceTaskIDs,
		Metadata:         m.Metadata,
	}, nil
}

// Message rebuilds the A2A message an envelope came from. It is the inverse of
// EnvelopeFromMessage, and it is what a crier→A2A projection (a pushed reply, an
// artifact published on the relay) goes through so there is exactly one place
// that knows how a part is shaped.
func (e Envelope) Message() (*Message, error) {
	parts := make([]Part, 0, len(e.Parts))
	for i, mp := range e.Parts {
		p, err := partToA2A(mp, fmt.Sprintf("parts[%d]", i))
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return &Message{
		MessageID:        e.MessageID,
		ContextID:        e.ContextID,
		TaskID:           e.TaskID,
		Role:             e.Role,
		Parts:            parts,
		Metadata:         e.Metadata,
		Extensions:       e.Extensions,
		ReferenceTaskIDs: e.ReferenceTaskIDs,
	}, nil
}

// ParseEnvelope decodes a crier payload as an Envelope. ok=false means the
// payload is not one of this binding's envelopes (no `parts` array), which is
// the normal case for a payload an A2A client's own agent sent.
func ParseEnvelope(payload json.RawMessage) (Envelope, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Envelope{}, false
	}
	var probe struct {
		Parts *[]MessagePart `json:"parts"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil || probe.Parts == nil {
		return Envelope{}, false
	}
	var env Envelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return Envelope{}, false
	}
	return env, true
}

// PartsFromPayload projects an arbitrary crier payload onto A2A parts: an
// envelope's own parts when the payload IS one of this binding's envelopes,
// otherwise the payload verbatim as a single data part. Nothing is dropped and
// nothing is re-encoded — an opaque payload stays opaque, which is what makes
// this safe to apply to a reply body or a relay event this server did not
// produce.
func PartsFromPayload(payload json.RawMessage) ([]Part, error) {
	if env, ok := ParseEnvelope(payload); ok {
		parts := make([]Part, 0, len(env.Parts))
		for i, mp := range env.Parts {
			p, err := partToA2A(mp, fmt.Sprintf("payload.parts[%d]", i))
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
		}
		if len(parts) > 0 {
			return parts, nil
		}
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, nil
	}
	return []Part{{Data: append(json.RawMessage(nil), trimmed...)}}, nil
}

// partFromA2A projects ONE A2A part onto its crier form, refusing everything the
// projection cannot carry faithfully. field is the parameter path the refusal
// names.
func partFromA2A(p Part, field string) (MessagePart, error) {
	content := p.contentMembers()
	switch len(content) {
	case 0:
		return MessagePart{}, invalidParams(field,
			"exactly one of text, raw, url or data is required")
	case 1:
	default:
		return MessagePart{}, invalidParams(field,
			"exactly one of text, raw, url or data is allowed, got %d (%v)", len(content), content)
	}

	mp := MessagePart{
		MediaType: p.MediaType,
		Filename:  p.Filename,
		Metadata:  p.Metadata,
	}
	alt, tags, caption, err := liftPartMetadata(p.Metadata, field)
	if err != nil {
		return MessagePart{}, err
	}
	mp.Alt, mp.Tags, mp.Caption = alt, tags, caption

	switch content[0] {
	case "text":
		mp.Type = PartTypeText
		mp.Text = *p.Text
	case "data":
		mp.Type = PartTypeData
		mp.Data = append(json.RawMessage(nil), bytes.TrimSpace(p.Data)...)
	case "url":
		mp.Type = PartTypeFile
		mp.File = &FileRef{URI: *p.URL}
	case "raw":
		decoded, err := base64.StdEncoding.DecodeString(*p.Raw)
		if err != nil {
			return MessagePart{}, invalidParams(field+".raw",
				"is not valid base64, which is how an A2A file part carries inline bytes")
		}
		sum := sha256.Sum256(decoded)
		mp.Type = PartTypeFile
		mp.File = &FileRef{
			Bytes: *p.Raw,
			Size:  len(decoded),
			Hash:  hashPrefix + hex.EncodeToString(sum[:]),
		}
	}
	return mp, nil
}

// liftPartMetadata reads the three metadata members this binding lifts onto the
// crier part's fields, refusing a wrongly-typed one rather than coercing it
// (a `tags` that is a string is a client bug this binding will not hide).
// Unknown metadata members are not this function's business: they ride in the
// part's `metadata` untouched.
func liftPartMetadata(meta map[string]any, field string) (alt string, tags []string, caption string, err error) {
	if v, ok := meta[metaAlt]; ok {
		s, ok := v.(string)
		if !ok {
			return "", nil, "", invalidParams(field+".metadata.alt", "must be a string, got %T", v)
		}
		alt = s
	}
	if v, ok := meta[metaTags]; ok {
		list, ok := v.([]any)
		if !ok {
			return "", nil, "", invalidParams(field+".metadata.tags", "must be an array of strings, got %T", v)
		}
		tags = make([]string, 0, len(list))
		for i, item := range list {
			s, ok := item.(string)
			if !ok {
				return "", nil, "", invalidParams(fmt.Sprintf("%s.metadata.tags[%d]", field, i),
					"must be a string, got %T", item)
			}
			tags = append(tags, s)
		}
	}
	if v, ok := meta[metaCaption]; ok {
		s, ok := v.(string)
		if !ok {
			return "", nil, "", invalidParams(field+".metadata.caption", "must be a string, got %T", v)
		}
		caption = s
	}
	return alt, tags, caption, nil
}

// partToA2A is the inverse projection: a crier message part back onto an A2A
// part, including the metadata the lift above took apart.
//
// `role` is deliberately NOT restored per part: A2A's Role is message-scoped
// (§4.1.5) and the envelope carries it once (see the file comment).
func partToA2A(mp MessagePart, field string) (Part, error) {
	p := Part{Filename: mp.Filename, MediaType: mp.MediaType, Metadata: mp.Metadata}

	switch mp.Type {
	case PartTypeText:
		text := mp.Text
		p.Text = &text
	case PartTypeData:
		if len(bytes.TrimSpace(mp.Data)) == 0 {
			return Part{}, invalidParams(field+".data", "a data part must carry a value")
		}
		p.Data = append(json.RawMessage(nil), bytes.TrimSpace(mp.Data)...)
	case PartTypeFile:
		if mp.File == nil {
			return Part{}, invalidParams(field+".file", "a file part must carry a reference or inline bytes")
		}
		switch {
		case mp.File.URI != "":
			uri := mp.File.URI
			p.URL = &uri
		case mp.File.Bytes != "":
			raw := mp.File.Bytes
			p.Raw = &raw
		default:
			return Part{}, invalidParams(field+".file", "a file part must carry uri or bytes")
		}
	default:
		return Part{}, invalidParams(field+".type", "must be %s, %s or %s, got %q",
			PartTypeText, PartTypeFile, PartTypeData, mp.Type)
	}

	// The lifted members go back into metadata so the A2A view is whole again;
	// metadata's own copies (if any) are overwritten by the part's, which are
	// the ones a crier consumer could have edited.
	if mp.Alt != "" || len(mp.Tags) > 0 || mp.Caption != "" {
		if p.Metadata == nil {
			p.Metadata = map[string]any{}
		}
		if mp.Alt != "" {
			p.Metadata[metaAlt] = mp.Alt
		}
		if len(mp.Tags) > 0 {
			tags := make([]any, 0, len(mp.Tags))
			for _, t := range mp.Tags {
				tags = append(tags, t)
			}
			p.Metadata[metaTags] = tags
		}
		if mp.Caption != "" {
			p.Metadata[metaCaption] = mp.Caption
		}
	}
	if len(p.Metadata) == 0 {
		p.Metadata = nil
	}
	return p, nil
}

// decodeParts decodes a `parts` array strictly: every member must be a Part the
// model declares, and the oneof is checked by the caller's projection.
func decodeParts(raw json.RawMessage, field string) ([]Part, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalidParams(field, "is required")
	}
	var parts []Part
	if err := strictDecode(raw, &parts); err != nil {
		return nil, invalidParams(field, "%s", err.Error())
	}
	if len(parts) == 0 {
		return nil, invalidParams(field, "at least one part is required")
	}
	return parts, nil
}
