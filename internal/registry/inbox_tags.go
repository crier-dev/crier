package registry

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/crier-dev/crier/internal/tags"
)

// Inbox tags (COURIER-TAGS §2.4, §3): a tag rides the ROUTE RECORD, so the
// inbox entry a message fans out to carries the same classification the
// transcript does. That is what lets `GET /agents/{id}/inbox?tag=build` answer
// for a QUEUED message without reading the session, which may be in another
// realm — or already gone.
//
// The column is additive (migration 019): a pre-existing row stores NULL and
// reads back as untagged, exactly like a message record written before the
// field existed.

// tagsJSONB encodes an entry's tags for the store. The empty list stores SQL
// NULL — "never tagged" has ONE representation, and an absent tag list must
// not become an empty JSON array on the wire (§6).
func tagsJSONB(e *InboxEntry) any {
	if e == nil || len(e.Tags) == 0 {
		return nil
	}
	raw, err := json.Marshal(e.Tags)
	if err != nil {
		return nil
	}
	return string(raw)
}

// applyTagsJSONB decodes a stored tags column back onto the entry. NULL and an
// unparseable body both leave the entry untagged rather than failing the read
// that carries the message — the same degradation posture the location and
// selection columns take: corrupt classification metadata must never make a
// delivered message unreadable.
func applyTagsJSONB(e *InboxEntry, raw []byte) {
	if e == nil || len(raw) == 0 {
		return
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return
	}
	if len(out) > 0 {
		e.Tags = out
	}
}

// inboxTagFilter parses the ?tag= (AND) and ?tags_any= (OR) parameters of the
// inbox read. It delegates to the same parser the session read routes use, so
// the two surfaces cannot disagree about what a filter means — including
// refusing BOTH spellings at once (tags.ErrFilterConflict).
func inboxTagFilter(r *http.Request) (tags.Filter, error) {
	q := r.URL.Query()
	return tags.ParseFilter(q["tag"], q["tags_any"])
}

// writeTagFilterError answers a refused filter with the §5 code in the
// registry surface's own error envelope. The codes are the catalog's
// (`TAGS_FILTER_CONFLICT`, `TAGS_INVALID`), not a second vocabulary: a client
// branching on the session surface's code branches on the same one here.
func writeTagFilterError(w http.ResponseWriter, err error) {
	code := "TAGS_INVALID"
	if errors.Is(err, tags.ErrFilterConflict) {
		code = "TAGS_FILTER_CONFLICT"
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "detail": err.Error()})
}
