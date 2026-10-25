//go:build integration

package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// PostgreSQL retains the original DDL, including database-level validation:
// bypass Go's trust validators to prove the dialect fix did not weaken it.
func TestPostgresMigration017Constraints(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	key, sig := strings.Repeat("a1", 32), strings.Repeat("b2", 64)
	for _, bad := range []string{"", "abc", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("A", 64)} {
		_, err := s.pool.Exec(t.Context(), `INSERT INTO trust_anchors VALUES('bad',$1,'label',now())`, bad)
		require.Error(t, err, "invalid public_key %q", bad)
		_, err = s.pool.Exec(t.Context(), `INSERT INTO trust_delegations VALUES($1,$2,'{}')`, sig, bad)
		require.Error(t, err, "invalid subject_key %q", bad)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("b", 127), strings.Repeat("b", 129), strings.Repeat("g", 128), strings.Repeat("B", 128)} {
		_, err := s.pool.Exec(t.Context(), `INSERT INTO trust_delegations VALUES($1,$2,'{}')`, bad, key)
		require.Error(t, err, "invalid signature %q", bad)
	}
	for _, bad := range []string{"", "{", "[]", "null", `"text"`, "1", "true"} {
		_, err := s.pool.Exec(t.Context(), `INSERT INTO trust_delegations VALUES($1,$2,$3::jsonb)`, sig, key, bad)
		require.Error(t, err, "invalid object record %q", bad)
	}
	for _, query := range []string{
		`INSERT INTO trust_anchors VALUES(NULL,'` + key + `','label',now())`,
		`INSERT INTO trust_anchors VALUES(' ','` + key + `','label',now())`,
		`INSERT INTO trust_anchors VALUES('id','` + key + `',' ',now())`,
		`INSERT INTO trust_delegations VALUES(NULL,'` + key + `','{}')`,
		`INSERT INTO trust_delegations VALUES('` + sig + `','` + key + `',NULL)`,
	} {
		_, err := s.pool.Exec(t.Context(), query)
		require.Error(t, err, query)
	}
	_, err := s.pool.Exec(t.Context(), `INSERT INTO trust_anchors VALUES('valid',$1,'label',now())`, key)
	require.NoError(t, err)
	_, err = s.pool.Exec(t.Context(), `INSERT INTO trust_anchors VALUES('duplicate',$1,'label',now())`, key)
	require.Error(t, err, "public_key remains unique")
	_, err = s.pool.Exec(t.Context(), `INSERT INTO trust_delegations VALUES($1,$2,'{}')`, sig, key)
	require.NoError(t, err)
}
