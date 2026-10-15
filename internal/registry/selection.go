package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file is the optional-selection-hint half of CR-FEAT-036: the two
// members a lane dispatch may carry so that a ROUTER (a task-router in a
// bunker, which cannot be reached inbound and therefore polls its own inbox)
// can choose the model/provider pair it will run on.
//
// Crier's share of that architecture is transport and preservation ONLY:
//
//   - it carries `model` and `provider` through the delivery path untouched,
//     stores them with the message and hands them back on every read of that
//     message (retrieve/lease, peek, list);
//   - it NEVER substitutes a concrete model or provider. The four selection
//     cases the consumer contract names — explicit/explicit, default/explicit,
//     explicit/default, default/default — are distinguished by WHAT THE CALLER
//     SENT, never by a value Crier invents;
//   - prompt analysis, model/provider selection and the Hermes Gateway call are
//     the task-router's work, not the bus's.
//
// A selection hint is DEFAULT in three supported input spellings. Omission is
// the canonical spelling callers should prefer, not a rewrite of every input:
//
//	absent   — the member is not in the request body at all (Go: nil pointer,
//	           wire: no key). This IS the canonical default.
//	null     — the member is present and JSON null. It resolves to the
//	           canonical default: a member with no value means "no value", so
//	           the stored and retrieved message carries the member ABSENT, and
//	           there is no separate null form in an inbox envelope.
//	blank    — the member is present and a string that is empty or whitespace
//	           only ("" is the canonical spelling of this arm). A blank string
//	           IS a value the caller wrote, so it is preserved as sent. Consumers
//	           must treat a present blank as default too (IsDefault).
//
// An explicit value is passed through byte-for-byte: no trimming, no case
// folding, no validation against a catalogue of models or providers. The bus
// does not know which model names exist, and a bus that rejected one would be
// a selector in disguise.
//
// What Crier guarantees for EVERY default spelling is the thing that matters:
// no concrete model or provider is ever filled in for them, so the router sees
// the choice is still open. Distinct explicit values, on the other hand, are
// never collapsed together — preserving the caller's pair is what lets a
// non-router consumer (one that already knows which model and provider it
// wants) use the same envelope without a router in the path.
type HintForm string

// The forms a selection hint can be observed in. HintAbsent is never stored on
// a value: an absent hint is a nil *SelectionHint (which is also what an
// omitted JSON member and an explicit JSON null both decode to). HintNull only
// appears on a hint value that was decoded outside the delivery envelope (a
// non-pointer position), where the JSON null reaches UnmarshalJSON; on the
// delivery path null resolves to HintAbsent.
const (
	HintAbsent   HintForm = "absent"
	HintNull     HintForm = "null"
	HintBlank    HintForm = "blank"
	HintExplicit HintForm = "explicit"
)

// SelectionHint is one optional model/provider selection hint of a dispatch
// (CR-FEAT-036), carrying BOTH halves of the contract its consumer needs: the
// value as the caller wrote it (Value) and the form it was observed in (Form),
// so a router can tell "the caller chose this" from "the caller left it open".
//
// It is a named type rather than a bare `*string` so that the classification
// lives in ONE place instead of being re-derived by every consumer: IsDefault
// and Selects are the decision a router makes, and the comment below is the
// contract behind it. A *string would leave "is a blank model a choice?"
// answerable only by convention, and the answer is not obvious — a blank model
// name is not a model.
//
// The JSON encoding reproduces the caller's spelling: the string exactly as
// sent (an empty string for a blank hint) and JSON null for the null form,
// which only a hint decoded outside the delivery envelope can be (see
// UnmarshalJSON).
type SelectionHint struct {
	// Form is how the hint arrived: HintNull, HintBlank or HintExplicit.
	// HintAbsent is represented by the nil pointer, never by a value.
	Form HintForm `json:"-"`
	// Value is the hint exactly as the caller sent it. Empty for HintNull.
	Value string `json:"-"`
}

// MarshalJSON renders the hint in the form it arrived in: JSON null for the
// null form (reachable only outside the delivery envelope) and the string
// otherwise, so a blank hint goes out as an empty string, never rewritten.
func (h SelectionHint) MarshalJSON() ([]byte, error) {
	if h.Form == HintNull {
		return []byte("null"), nil
	}
	return json.Marshal(h.Value)
}

// UnmarshalJSON accepts the two shapes a documented hint may take — a JSON
// string and JSON null — and refuses anything else, so a malformed hint
// (`"model": 5`, `"model": {}`) is a loud 400 at the HTTP boundary rather than
// a silently dropped selection.
//
// The null arm is for a hint decoded in a NON-POINTER position: in the
// delivery envelope the members are `*SelectionHint` (so an absent member and
// an explicit null both resolve to the canonical default — the nil pointer),
// but this type must stay total wherever a consumer embeds it by value.
func (h *SelectionHint) UnmarshalJSON(b []byte) error {
	if h == nil {
		return fmt.Errorf("selection hint: nil receiver")
	}
	if strings.TrimSpace(string(b)) == "null" {
		*h = SelectionHint{Form: HintNull}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("selection hint must be a string or null: %w", err)
	}
	*h = SelectionHint{Value: s}
	if strings.TrimSpace(s) == "" {
		h.Form = HintBlank
	} else {
		h.Form = HintExplicit
	}
	return nil
}

// IsDefault reports whether this hint leaves the choice to the consumer: a nil
// hint (the member was absent), an explicit JSON null, or a blank string. A
// value built without a Form (the zero struct) is judged by its Value, so
// `&SelectionHint{}` reads as the default rather than as an explicit empty
// model name.
func (h *SelectionHint) IsDefault() bool {
	if h == nil {
		return true
	}
	switch h.Form {
	case HintNull, HintBlank, HintAbsent:
		return true
	case HintExplicit:
		return false
	}
	return strings.TrimSpace(h.Value) == ""
}

// Selects reports whether the hint names a concrete value — the negation of
// IsDefault, named for the reader who is asking "did the caller choose?".
func (h *SelectionHint) Selects() bool { return !h.IsDefault() }

// String renders the original value, or "" for an absent hint. In particular,
// whitespace-only defaults retain their whitespace; use IsDefault to classify.
func (h *SelectionHint) String() string {
	if h == nil {
		return ""
	}
	return h.Value
}

// NewHint builds an EXPLICIT hint from a concrete value — the constructor a
// consumer or a test uses for the "the caller chose this" arm. A blank value
// yields the blank (default) form, because that is what the wire would say.
func NewHint(value string) *SelectionHint {
	h := &SelectionHint{Value: value, Form: HintExplicit}
	if strings.TrimSpace(value) == "" {
		h.Form = HintBlank
	}
	return h
}

// selectionState is the JSONB body stored in inbox_entries.selection. It holds
// the two optional strings in one column. Absent/null decode to nil; blanks
// survive unchanged. The wire exposes these as top-level entry members.
type selectionState struct {
	Model    *SelectionHint `json:"model,omitempty"`
	Provider *SelectionHint `json:"provider,omitempty"`
}

// selectionJSONB renders an entry's optional hints for the
// inbox_entries.selection column, or nil (SQL NULL) when the delivery named
// neither hint — so a hint-less message is stored exactly as it was before the
// column existed, and "not sent" has one representation.
func selectionJSONB(e *InboxEntry) any {
	if e == nil || (e.Model == nil && e.Provider == nil) {
		return nil
	}
	raw, err := json.Marshal(selectionState{Model: e.Model, Provider: e.Provider})
	if err != nil {
		return nil
	}
	return string(raw)
}

// applySelectionJSONB decodes a stored selection column back onto the entry.
// NULL (nothing was sent) leaves both hints nil. An unparseable body also
// leaves them nil rather than failing the read that carries the message — the
// same degradation posture the location column takes: a corrupt metadata
// column must not make a delivered message unreadable.
func applySelectionJSONB(e *InboxEntry, raw []byte) {
	if e == nil || len(raw) == 0 {
		return
	}
	var st selectionState
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	if st.Model != nil {
		e.Model = st.Model
	}
	if st.Provider != nil {
		e.Provider = st.Provider
	}
}
