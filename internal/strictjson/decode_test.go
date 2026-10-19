package strictjson

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDecodeAcceptedListFollowsStruct(t *testing.T) {
	type request struct {
		First  string `json:"first,omitempty"`
		Added  bool   `json:"new_field"`
		Skip   string `json:"-"`
		hidden string
	}
	var req request
	require.EqualError(t, Decode(strings.NewReader(`{"typo":true}`), &req), `unknown field "typo" (accepted: first, new_field)`)
	require.NoError(t, Decode(strings.NewReader(`{"FIRST":"ok","new_field":true}`), &req))
	require.Equal(t, "ok", req.First)
	require.True(t, req.Added)
	require.Empty(t, req.hidden, "unexported fields are not JSON fields")
}

func TestDecodeNestedAndCustomFields(t *testing.T) {
	type child struct {
		Name string `json:"name"`
	}
	type request struct {
		Children []child         `json:"children"`
		When     time.Time       `json:"when"`
		Payload  json.RawMessage `json:"payload"`
		Metadata map[string]any  `json:"metadata"`
	}
	for _, tc := range []struct{ body, want string }{
		{`{"children":[{"typo":true}]}`, `children: element 0: unknown field "typo" (accepted: name)`},
		{`{"unknown":true,"later":true}`, `unknown field "unknown" (accepted: children, when, payload, metadata)`},
		{`{"children":null,"when":"2026-10-19T00:00:00Z","payload":{"anything":true},"metadata":{"any_key":1}}`, ""},
		{`null`, ""},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var req request
			err := Decode(strings.NewReader(tc.body), &req)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.want)
			}
		})
	}
}

func TestDecodeRetainsJSONTypeErrors(t *testing.T) {
	for _, raw := range []string{``, `{`, `[]`, `{"count":"text"}`, `{"when":"not-a-date"}`} {
		var req struct {
			Count int       `json:"count"`
			When  time.Time `json:"when"`
		}
		require.Error(t, Decode(strings.NewReader(raw), &req), raw)
	}
}
