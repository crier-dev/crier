package chat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The Message struct in kinds.go carries a `json:"kind"` tag with no
// omitempty, so the kind field is always present on the wire for all three
// kinds (plain, addressed, task) and JSON round-trips preserve it.

// RenderTranscript renders a message for a transcript, visibly distinguishing
// the kinds:
//
//	PLAIN      <from>: <body>
//	ADDRESSED  <from> → @a,@b: <body>
//	TASK       <from> [TASK <state>]: <body>
func RenderTranscript(m Message) string {
	switch m.Kind {
	case KindAddressed:
		return fmt.Sprintf("%s → %s: %s", m.From, strings.Join(m.Addressees, ","), m.Body)
	case KindTask:
		return fmt.Sprintf("%s [TASK %s]: %s", m.From, m.TaskState, m.Body)
	default:
		return fmt.Sprintf("%s: %s", m.From, m.Body)
	}
}

// MarshalMessage is a thin wrapper over json.Marshal kept as a named seam for
// callers so wire usage of Message is explicit and greppable.
func MarshalMessage(m Message) ([]byte, error) {
	return json.Marshal(m)
}

// UnmarshalMessage is the inverse of MarshalMessage. The kind field must be
// present and valid on the wire.
func UnmarshalMessage(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, err
	}
	switch m.Kind {
	case KindPlain, KindAddressed, KindTask:
	default:
		return Message{}, ErrInvalidKind
	}
	return m, nil
}
