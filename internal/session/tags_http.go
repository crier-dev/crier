package session

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/tags"
)

// ---------------------------------------------------------------------------
// CRIER-TAGS (CRIER-TAGS-1): the write-path grammar gate, the read filters
// and the vocabulary endpoint.
//
// A tag CLASSIFIES a message; it never routes and never grants (§1). Everything
// here is therefore additive to the existing routes: a message without tags
// behaves exactly as it did, and a read without a filter returns the same
// transcript, search result or inbox batch it returned before this row.
// ---------------------------------------------------------------------------

// normalizeMessageTags validates a caller-supplied tag list through
// tags.Normalize and answers the §5 error codes for a refusal.
//
// The reserved `sys:` prefix is refused for EVERY caller in this build: the
// spec admits it "only from an operator principal" (§2.1), and this server has
// no operator principal — every credential it accepts is an agent key or an
// unauthenticated claim. Refusing the prefix outright is the strict subset of
// the spec's rule, and it is stated here rather than silently allowing writes
// the spec gives only to operators.
func (h *Handler) normalizeMessageTags(w http.ResponseWriter, raw []string) ([]string, bool) {
	normalized, err := tags.Normalize(raw)
	if err != nil {
		writeTagError(w, err)
		return nil, false
	}
	if tag, reserved := tags.ReservedViolation(normalized); reserved {
		writeAPIError(w, http.StatusForbidden, "TAGS_RESERVED_PREFIX",
			fmt.Sprintf("tag %q uses the reserved %q prefix, which only an operator principal may write", tag, tags.ReservedPrefix))
		return nil, false
	}
	return normalized, true
}

// writeTagError maps a tags-package refusal onto the §5 catalog. TAGS_TOO_MANY
// is checked first because it is the more specific answer: a 17-tag list is
// also a list this server will not store, and naming the count is more useful
// than naming a grammar rule it did not break.
func writeTagError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tags.ErrTooMany):
		writeAPIError(w, http.StatusBadRequest, "TAGS_TOO_MANY", err.Error())
	case errors.Is(err, tags.ErrFilterConflict):
		writeAPIError(w, http.StatusBadRequest, "TAGS_FILTER_CONFLICT", err.Error())
	case errors.Is(err, tags.ErrQueryTooBroad):
		writeAPIError(w, http.StatusBadRequest, "TAGS_QUERY_TOO_BROAD", err.Error())
	default:
		writeAPIError(w, http.StatusBadRequest, "TAGS_INVALID", err.Error())
	}
}

// readTagFilter parses the ?tag= (AND) and ?tags_any= (OR) parameters of a read
// route. It is the ONE place the two spellings are interpreted, so the
// transcript, the search and the inbox cannot disagree about them — including
// about refusing both at once with TAGS_FILTER_CONFLICT (§2.4).
func readTagFilter(w http.ResponseWriter, r *http.Request) (tags.Filter, bool) {
	q := r.URL.Query()
	filter, err := tags.ParseFilter(q["tag"], q["tags_any"])
	if err != nil {
		writeTagError(w, err)
		return tags.Filter{}, false
	}
	return filter, true
}

// ---------------------------------------------------------------------------
// GET /tags — the category vocabulary (§2.3).
// ---------------------------------------------------------------------------

// tagVocabularyEntry is one vocabulary row: the tag, how many messages carry it
// and when it was last used. A tag that has never been used has no row at all —
// there is no vocabulary table, so a rename is simply a new tag (§6).
type tagVocabularyEntry struct {
	Tag      string    `json:"tag"`
	Count    int       `json:"count"`
	LastUsed time.Time `json:"last_used"`
}

type tagsResponse struct {
	Tags []tagVocabularyEntry `json:"tags"`
}

// HandleListTags serves GET /tags: the tags this caller can actually see, with
// per-tag counts and the newest message that carries each one (§2.3).
//
// Scope is the caller's READ visibility, not the whole database. The realm rule
// is the one every session read applies — a request's namespace (the
// X-Crier-Namespace header, or the `namespace` query parameter) selects the
// realm, and a session outside it is not counted. Inside the realm,
// authorizeReadAs decides per session: a `private` session the caller is
// neither a member of nor granted read on contributes nothing to a count. That
// is the same gate HandleTranscript applies, so a reader can never learn from
// the vocabulary that a tag exists only on a message it may not open.
func (h *Handler) HandleListTags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	realm, err := h.realm(r, q.Get("namespace"))
	if err != nil {
		writeRealmError(w, err)
		return
	}

	limit := 0 // 0 = every tag in scope; a positive limit truncates after sorting.
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 1 {
			writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a positive integer")
			return
		}
		if parsed > tags.MaxVocabulary {
			writeTagError(w, fmt.Errorf("%w: limit %d exceeds the maximum %d", tags.ErrQueryTooBroad, parsed, tags.MaxVocabulary))
			return
		}
		limit = parsed
	}

	var since time.Time
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		parsed, timeErr := time.Parse(time.RFC3339, raw)
		if timeErr != nil {
			writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "since must be an RFC 3339 timestamp")
			return
		}
		since = parsed.UTC()
	}

	ids, err := h.store.Sessions(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	who := callerIdentity(r)
	type aggregate struct {
		count int
		last  time.Time
	}
	agg := make(map[string]*aggregate)
	for _, id := range ids {
		st, err := h.store.Load(r.Context(), id)
		if err != nil {
			// A session the store lists but cannot load is a real
			// inconsistency; it is surfaced rather than skipped so a count is
			// never quietly wrong (the same rule HandleListSessions applies).
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR",
				fmt.Sprintf("load session %q: %v", id, err))
			return
		}
		if st.Session == nil || namespace.Canonical(st.Session.Namespace) != realm {
			continue
		}
		// The permission gate: a session this caller may not read contributes
		// nothing. Counting it would leak the existence of a tag AND of the
		// private room it lives in.
		if err := h.authorizeReadAs(r.Context(), who, st.Session, st); err != nil {
			continue
		}
		for _, m := range st.Messages {
			if !since.IsZero() && m.CreatedAt.Before(since) {
				continue
			}
			for _, tag := range m.Tags {
				entry, ok := agg[tag]
				if !ok {
					entry = &aggregate{}
					agg[tag] = entry
				}
				entry.count++
				if m.CreatedAt.After(entry.last) {
					entry.last = m.CreatedAt
				}
			}
		}
	}

	out := tagsResponse{Tags: make([]tagVocabularyEntry, 0, len(agg))}
	for tag, entry := range agg {
		out.Tags = append(out.Tags, tagVocabularyEntry{Tag: tag, Count: entry.count, LastUsed: entry.last.UTC()})
	}
	// Most-used first, then by name: a stable order so two identical requests
	// answer identically (a map iteration is not an order).
	sort.Slice(out.Tags, func(i, j int) bool {
		if out.Tags[i].Count != out.Tags[j].Count {
			return out.Tags[i].Count > out.Tags[j].Count
		}
		return out.Tags[i].Tag < out.Tags[j].Tag
	})
	if limit > 0 && len(out.Tags) > limit {
		out.Tags = out.Tags[:limit]
	}
	writeJSON(w, http.StatusOK, out)
}
