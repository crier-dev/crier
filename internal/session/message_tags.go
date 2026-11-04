package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// The SQL view stores a message's tags (COURIER-TAGS §2.1) as one JSON array in
// a TEXT column on BOTH engines: SQLite as TEXT, PostgreSQL as TEXT rather than
// TEXT[]. That is deliberate, and it is what keeps ONE scan path serving both
// engines exactly as the rest of this store does — an array column would fork
// the codec (a `[]string` on pgx, a string on SQLite) and buy nothing here,
// because filtering is done in Go over the fetched set (§2.5), so no index or
// array operator is ever consulted. Absent and empty are the SAME stored value:
// SQL NULL, which reads back as no tags (§6).
//
// The column is additive (`ALTER TABLE ... ADD COLUMN`), so a pre-existing
// deployment keeps every row it had and an untagged historical message reads as
// untagged.

// encTags encodes tags for storage, mapping the empty list to SQL NULL so that
// "never tagged" has exactly one representation.
func encTags(list []string) any {
	if len(list) == 0 {
		return nil
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		// A []string cannot fail to marshal; refusing rather than storing a
		// half value is the safe direction if that ever changes.
		return nil
	}
	return string(encoded)
}

// decTags decodes a stored tags column. NULL, an empty value and the JSON
// literal `null` all read back as no tags, so a historical row and a
// never-tagged row are indistinguishable to a reader (§6).
func decTags(stored sql.NullString) ([]string, error) {
	if !stored.Valid || stored.String == "" || stored.String == "null" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(stored.String), &out); err != nil {
		return nil, fmt.Errorf("load message tags: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// decTagsPtr is decTags for a driver that surfaces NULL as a nil string pointer
// (the pgx pool store).
func decTagsPtr(stored *string) ([]string, error) {
	if stored == nil {
		return nil, nil
	}
	return decTags(sql.NullString{String: *stored, Valid: true})
}
