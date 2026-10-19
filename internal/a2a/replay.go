package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

// Replay retains the original send projection, not the retry's possibly changed
// payload/configuration. It is a receipt, not a new task-lifecycle observation.
type Replay struct {
	Translation *Translation  `json:"translation"`
	Accept      DeliverAccept `json:"accept"`
	AcceptedAt  time.Time     `json:"accepted_at"`
}

// DecodeReplay preserves numbers in free-form message/part metadata just as
// the original strict send decoder does. float64 would corrupt large ids.
func DecodeReplay(raw []byte) (*Replay, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid A2A replay receipt JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var receipt Replay
	if err := decoder.Decode(&receipt); err != nil {
		return nil, err
	}
	if receipt.Translation == nil || receipt.Translation.Request == nil || receipt.Accept.ID == "" || receipt.AcceptedAt.IsZero() {
		return nil, errors.New("incomplete A2A replay receipt")
	}
	return &receipt, nil
}
