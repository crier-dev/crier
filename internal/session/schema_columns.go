package session

import "strings"

// sessionAddedColumns are the chat_sessions columns introduced after the first
// schema. They are declared in every CREATE TABLE (so a fresh database gets
// them) AND applied as ALTERs here (so a database created before this feature
// reaches them). The ALTER is expected to fail with "duplicate column" on a
// fresh database; that error is the idempotency, not a fault.
//
// Adding a column here is the supported way to grow the view without a
// migration framework the session backends do not have.
var sessionAddedColumns = []string{
	`ALTER TABLE chat_sessions ADD COLUMN harness TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_sessions ADD COLUMN harness_session_id TEXT NOT NULL DEFAULT ''`,
}

// isDuplicateColumnError recognises the two engines' "the column is already
// there" classes: SQLite says "duplicate column name", PostgreSQL says
// "column ... already exists" (SQLSTATE 42701).
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
}
