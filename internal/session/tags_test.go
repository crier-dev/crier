package session

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CRIER-TAGS (CRIER-TAGS-1) §7: tags on the session API surface.
//
// Every case here runs against BOTH local backends (JSONL and SQLite), because
// §2.5 makes the filter a Go-level predicate over the fetched set: if the two
// backends ever disagreed, it would be because a tag was not PERSISTED, not
// because a query was written twice.
// ---------------------------------------------------------------------------

// apiErrorBody is the surface's ONE error envelope (writeAPIError).
type apiErrorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// send posts one message with tags and returns its view.
func (h *apiHarness) send(t *testing.T, sessionID, sender, text string, tags ...string) transcriptMessage {
	t.Helper()
	var sent transcriptMessage
	body := postMessageRequest{
		Payload: json.RawMessage(`{"text":"` + text + `"}`),
		Sender:  sender,
		Tags:    tags,
	}
	code := h.do(t, http.MethodPost, "/sessions/"+sessionID+"/messages", body, &sent, nil)
	require.Equal(t, http.StatusCreated, code, "send %q with tags %v", text, tags)
	return sent
}

// transcript fetches the (optionally filtered) transcript and returns its views.
func (h *apiHarness) transcript(t *testing.T, sessionID, query string) (transcriptResponse, int) {
	t.Helper()
	var out transcriptResponse
	path := "/sessions/" + sessionID + "/messages"
	if query != "" {
		path += "?" + query
	}
	code := h.do(t, http.MethodGet, path, nil, &out, nil)
	return out, code
}

func TestPostMessageStoresTags(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			sent := h.send(t, id, fixAgentA, "tagged", "build", "urgent", "build:ci")
			require.Equal(t, []string{"build", "urgent", "build:ci"}, sent.Tags, "the send response carries the tags")

			out, code := h.transcript(t, id, "")
			require.Equal(t, http.StatusOK, code)
			require.Len(t, out.Messages, 1)
			require.Equal(t, []string{"build", "urgent", "build:ci"}, out.Messages[0].Tags,
				"tags come back in the order they were supplied")
		})
	}
}

// TestPostMessageDedupesTags is §6: an exact repeat is stored once.
func TestPostMessageDedupesTags(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			sent := h.send(t, id, fixAgentA, "dup", "build", "build")
			require.Equal(t, []string{"build"}, sent.Tags)
		})
	}
}

// TestTranscriptFilterAND is §7.5: three messages tagged a, b and a+b; the AND
// filter returns only the message carrying both.
func TestTranscriptFilterAND(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			onlyA := h.send(t, id, fixAgentA, "a", "a")
			onlyB := h.send(t, id, fixAgentA, "b", "b")
			both := h.send(t, id, fixAgentA, "ab", "a", "b")

			out, code := h.transcript(t, id, "tag=a&tag=b")
			require.Equal(t, http.StatusOK, code)
			require.Len(t, out.Messages, 1)
			require.Equal(t, both.ID, out.Messages[0].ID)

			// One term is the ordinary case and must still work.
			out, code = h.transcript(t, id, "tag=a")
			require.Equal(t, http.StatusOK, code)
			require.Len(t, out.Messages, 2)
			got := []string{out.Messages[0].ID, out.Messages[1].ID}
			require.ElementsMatch(t, []string{onlyA.ID, both.ID}, got)
			require.NotContains(t, got, onlyB.ID)
		})
	}
}

// TestTranscriptFilterAny is §7.6: ?tags_any=a,b returns every message carrying
// either tag.
func TestTranscriptFilterAny(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			a := h.send(t, id, fixAgentA, "a", "a")
			b := h.send(t, id, fixAgentA, "b", "b")
			both := h.send(t, id, fixAgentA, "ab", "a", "b")
			untagged := h.send(t, id, fixAgentA, "none")

			out, code := h.transcript(t, id, "tags_any=a,b")
			require.Equal(t, http.StatusOK, code)
			require.Len(t, out.Messages, 3)
			ids := []string{out.Messages[0].ID, out.Messages[1].ID, out.Messages[2].ID}
			require.ElementsMatch(t, []string{a.ID, b.ID, both.ID}, ids,
				"the OR form returns every message carrying any of the terms")
			require.NotContains(t, ids, untagged.ID)
		})
	}
}

// TestUnknownTagFilterIsEmptyNot404 is §7.7 / §6: polling a category that has
// not appeared yet is an empty 200, never an error.
func TestUnknownTagFilterIsEmptyNot404(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			h.send(t, id, fixAgentA, "tagged", "build")

			out, code := h.transcript(t, id, "tag=nothing-yet")
			require.Equal(t, http.StatusOK, code)
			require.Equal(t, 0, out.Count)
			require.Empty(t, out.Messages)
			require.NotNil(t, out.Messages, "an empty result is an empty list, not a null")

			out, code = h.transcript(t, id, "tags_any=nothing-yet")
			require.Equal(t, http.StatusOK, code)
			require.Equal(t, 0, out.Count)
		})
	}
}

// TestTagFilterAndAnyConflict is §7.10: both spellings in one request is a
// named refusal, not a guess about precedence.
func TestTagFilterAndAnyConflict(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			h.send(t, id, fixAgentA, "tagged", "build")

			for _, path := range []string{
				"/sessions/" + id + "/messages?tag=build&tags_any=urgent",
				"/sessions/" + id + "/search?q=tagged&tag=build&tags_any=urgent",
			} {
				var failure apiErrorBody
				code := h.do(t, http.MethodGet, path, nil, &failure, nil)
				require.Equal(t, http.StatusBadRequest, code, path)
				require.Equal(t, "TAGS_FILTER_CONFLICT", failure.Error, path)
			}
		})
	}
}

// TestPostMessageRejectsInvalidTags is §5: a grammar violation is a 400 that
// names the offending tag, and nothing is recorded.
func TestPostMessageRejectsInvalidTags(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			cases := []struct {
				tags []string
				code string
			}{
				{tags: []string{"Build"}, code: "TAGS_INVALID"},
				{tags: []string{"-lead"}, code: "TAGS_INVALID"},
				{tags: []string{"a:b:c"}, code: "TAGS_INVALID"},
				{tags: []string{"build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "build", "over"}, code: "TAGS_TOO_MANY"},
				{tags: []string{"sys:heartbeat"}, code: "TAGS_RESERVED_PREFIX"},
			}
			for _, tc := range cases {
				var failure apiErrorBody
				code := h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
					postMessageRequest{Payload: json.RawMessage(`{"text":"no"}`), Sender: fixAgentA, Tags: tc.tags}, &failure, nil)
				want := http.StatusBadRequest
				if tc.code == "TAGS_RESERVED_PREFIX" {
					want = http.StatusForbidden
				}
				require.Equal(t, want, code, "tags %v", tc.tags)
				require.Equal(t, tc.code, failure.Error, "tags %v", tc.tags)
				if tc.code == "TAGS_INVALID" {
					require.Contains(t, failure.Detail, tc.tags[0], "the refusal names the offending tag")
				}
			}

			// Nothing was recorded by any of the refusals.
			out, code := h.transcript(t, id, "")
			require.Equal(t, http.StatusOK, code)
			require.Equal(t, 0, out.Count)
		})
	}
}

// TestSearchTagFilter is §2.4's second row: search narrows by tag, and the
// combined form (text AND tag) is the intersection.
func TestSearchTagFilter(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			build := h.send(t, id, fixAgentA, "deploy the build", "build")
			h.send(t, id, fixAgentA, "deploy the docs", "docs")

			var hits searchResponse
			code := h.do(t, http.MethodGet, "/sessions/"+id+"/search?q=deploy&tag=build", nil, &hits, nil)
			require.Equal(t, http.StatusOK, code)
			require.Equal(t, 1, hits.Count)
			require.Equal(t, build.ID, hits.Hits[0].MessageID)

			// Without the tag the same query sees both messages.
			code = h.do(t, http.MethodGet, "/sessions/"+id+"/search?q=deploy", nil, &hits, nil)
			require.Equal(t, http.StatusOK, code)
			require.Equal(t, 2, hits.Count)
		})
	}
}

// TestFanoutCarriesTagsToInbox is the bridge §2.4 needs: the tags a message is
// classified with ride the ROUTE RECORD, so the inbox entry a participant
// receives carries the same classification the transcript does — which is what
// makes `GET /agents/{id}/inbox?tag=` answerable for a queued message.
func TestFanoutCarriesTagsToInbox(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			sent := h.send(t, id, fixAgentA, "deploy", "build", "urgent")

			entries, _, err := h.reg.Retrieve(fixAgentB, time.Minute, 10)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, InboxEntryID(sent.ID, fixAgentB), entries[0].ID)
			require.Equal(t, []string{"build", "urgent"}, entries[0].Tags,
				"the route record carries the message's tags")
		})
	}
}

// TestTagsVocabularyCounts is §7.8: counts and last_used over the messages the
// caller may read.
func TestTagsVocabularyCounts(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			h.send(t, id, fixAgentA, "one", "build")
			h.send(t, id, fixAgentA, "two", "build", "urgent")
			h.send(t, id, fixAgentA, "three", "docs")
			h.send(t, id, fixAgentA, "four")

			var vocab tagsResponse
			code := h.do(t, http.MethodGet, "/tags", nil, &vocab, nil)
			require.Equal(t, http.StatusOK, code)
			counts := map[string]int{}
			for _, entry := range vocab.Tags {
				counts[entry.Tag] = entry.Count
				require.False(t, entry.LastUsed.IsZero(), "every counted tag has a last_used")
			}
			require.Equal(t, map[string]int{"build": 2, "urgent": 1, "docs": 1}, counts)
			require.Len(t, vocab.Tags, 3)
			require.Equal(t, "build", vocab.Tags[0].Tag, "most used first")
		})
	}
}

// TestTagsVocabularyPermissionScope is §7.8's permission case: a private
// session the caller cannot read contributes NOTHING to a count — otherwise the
// vocabulary would leak both the tag and the room.
func TestTagsVocabularyPermissionScope(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)

			// A private room whose only member is atlas, on the default realm.
			var created sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title:      "secret",
				Kind:       string(KindChannel),
				Visibility: VisibilityPrivate,
				CreatedBy:  &AuthorRef{Principal: fixCreator},
				Members: []addParticipantRequest{
					{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)},
				},
			}, &created, nil))
			h.send(t, created.ID, fixAgentA, "classified", "secret-tag")

			// An outsider sees no trace of it.
			var outsider tagsResponse
			code := h.do(t, http.MethodGet, "/tags", nil, &outsider, map[string]string{"X-Agent-ID": fixAgentB})
			require.Equal(t, http.StatusOK, code)
			for _, entry := range outsider.Tags {
				require.NotEqual(t, "secret-tag", entry.Tag, "a private room's tags are not counted for an outsider")
			}

			// The member sees it.
			var member tagsResponse
			code = h.do(t, http.MethodGet, "/tags", nil, &member, map[string]string{"X-Agent-ID": fixAgentA})
			require.Equal(t, http.StatusOK, code)
			found := false
			for _, entry := range member.Tags {
				if entry.Tag == "secret-tag" {
					found = true
					require.Equal(t, 1, entry.Count)
				}
			}
			require.True(t, found, "a member's own room is counted")
		})
	}
}

// TestTagsVocabularyLimitGuard is §5's TAGS_QUERY_TOO_BROAD.
func TestTagsVocabularyLimitGuard(t *testing.T) {
	t.Parallel()
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	t.Cleanup(func() { _ = store.Close() })
	h := newAPIHarness(t, store)
	for _, tc := range []struct {
		query string
		code  int
		error string
	}{
		{query: "?limit=1000", code: http.StatusOK},
		{query: "?limit=1001", code: http.StatusBadRequest, error: "TAGS_QUERY_TOO_BROAD"},
		{query: "?limit=0", code: http.StatusBadRequest, error: "INVALID_REQUEST"},
		{query: "?limit=abc", code: http.StatusBadRequest, error: "INVALID_REQUEST"},
		{query: "?since=not-a-time", code: http.StatusBadRequest, error: "INVALID_REQUEST"},
	} {
		var body json.RawMessage
		code := h.do(t, http.MethodGet, "/tags"+tc.query, nil, &body, nil)
		require.Equal(t, tc.code, code, tc.query)
		if tc.error != "" {
			var failure apiErrorBody
			require.NoError(t, json.Unmarshal(body, &failure))
			require.Equal(t, tc.error, failure.Error, tc.query)
		}
	}
}

// TestTagsSurviveReopen proves the tag is PERSISTED, not held in memory: the
// same store reopened from disk answers the same filtered transcript, and the
// historical untagged message still reads as untagged (§6, §7.11).
func TestTagsSurviveReopen(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			path := be.pathFn(t)
			store := be.open(t, path)
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			tagged := h.send(t, id, fixAgentA, "tagged", "build", "urgent")
			untagged := h.send(t, id, fixAgentA, "plain")
			require.NoError(t, store.Close())

			reopened := be.open(t, path)
			t.Cleanup(func() { _ = reopened.Close() })
			h2 := newAPIHarness(t, reopened)

			out, code := h2.transcript(t, id, "tag=urgent")
			require.Equal(t, http.StatusOK, code)
			require.Len(t, out.Messages, 1)
			require.Equal(t, tagged.ID, out.Messages[0].ID)
			require.Equal(t, []string{"build", "urgent"}, out.Messages[0].Tags)

			out, code = h2.transcript(t, id, "")
			require.Equal(t, http.StatusOK, code)
			require.Len(t, out.Messages, 2)
			for _, m := range out.Messages {
				if m.ID == untagged.ID {
					require.Empty(t, m.Tags, "an untagged message reads back untagged")
				}
			}

			var vocab tagsResponse
			code = h2.do(t, http.MethodGet, "/tags", nil, &vocab, nil)
			require.Equal(t, http.StatusOK, code)
			require.Len(t, vocab.Tags, 2)
		})
	}
}

// TestTagFilterBackendParity is §7.11: the same fixture produces the same
// filtered ids on every backend, because the filter is one Go predicate over
// each backend's fetched set.
func TestTagFilterBackendParity(t *testing.T) {
	t.Parallel()
	fixture := func(t *testing.T, be localBackend) map[string][]string {
		t.Helper()
		store := be.open(t, be.pathFn(t))
		t.Cleanup(func() { _ = store.Close() })
		h := newAPIHarness(t, store)
		id := h.createRoom(t)
		h.send(t, id, fixAgentA, "one", "build")
		h.send(t, id, fixAgentA, "two", "urgent")
		h.send(t, id, fixAgentA, "three", "build", "urgent")
		h.send(t, id, fixAgentA, "four")

		// Message ids are minted per run, so parity is compared on the TEXT of
		// the messages each filter selects — the same selection, in the same
		// order, on every backend.
		out := map[string][]string{}
		for _, query := range []string{"tag=build", "tag=urgent", "tag=build&tag=urgent", "tags_any=build,urgent", "tag=absent"} {
			resp, code := h.transcript(t, id, query)
			require.Equal(t, http.StatusOK, code, query)
			texts := []string{}
			for _, m := range resp.Messages {
				var payload struct {
					Text string `json:"text"`
				}
				require.NoError(t, json.Unmarshal(m.Payload, &payload))
				texts = append(texts, payload.Text)
			}
			out[query] = texts
		}
		return out
	}

	if len(localBackends()) < 2 {
		t.Skip("parity needs at least two local backends")
	}
	backends := localBackends()
	first := fixture(t, backends[0])
	for _, be := range backends[1:] {
		got := fixture(t, be)
		for query, ids := range first {
			require.Equal(t, ids, got[query], "%s disagrees with %s on %q", be.name, backends[0].name, query)
		}
	}
}
