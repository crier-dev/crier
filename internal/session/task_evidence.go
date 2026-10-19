package session

import (
	"encoding/json"
	"strings"
)

func taskActorKind(a AuthorRef) string {
	if a.Principal != "" {
		return "principal"
	}
	return "agent"
}

// Empty JSON containers, null, and blank text (including nested placeholders)
// are not completion evidence. Numbers remain meaningful: zero rows is a valid
// measurement when explicitly supplied in an evidence object.
func hasTaskEvidence(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return nonEmptyTaskEvidence(value)
}

func nonEmptyTaskEvidence(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		for _, item := range v {
			if nonEmptyTaskEvidence(item) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, item := range v {
			if nonEmptyTaskEvidence(item) {
				return true
			}
		}
		return false
	default:
		return true
	}
}
