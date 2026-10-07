package session

import (
	"fmt"
	"net/http"
	"sort"
)

// ---------------------------------------------------------------------------
// GET /sessions/{id}/output — the session-level DUAL OUTPUT read surface
// (CR-CHAT-031). The reader toggles, for the whole session, between:
//
//   mode=trace   (default) — the full ordered transcript with timestamps,
//                exactly the payload GET /sessions/{id}/messages serves. The
//                handler DELEGATES to HandleTranscript: one read path, never a
//                fork of it (the record is the trace, always).
//   mode=summary — the most recent generated summary state of the session's
//                threads: visibly marked generated, with a `covers` field
//                naming what it covers and a `trace` link back to the record
//                (one read underneath, never a replacement — §4.7 rule 2, the
//                same index rule the per-thread summary follows).
//
// A session with no threads has no summary to serve: the response is an
// honest 404 SUMMARY_UNAVAILABLE, never an empty or synthesised summary.
// ---------------------------------------------------------------------------

// sessionSummaryCovers names WHAT the summary covers — session id, the thread
// ids it indexes and the total message count across them.
type sessionSummaryCovers struct {
	SessionID    string   `json:"session_id"`
	ThreadIDs    []string `json:"thread_ids"`
	MessageCount int      `json:"message_count"`
}

// sessionSummaryResponse is the mode=summary body: the generated index plus
// the link to the record it indexes.
type sessionSummaryResponse struct {
	SessionID string               `json:"session_id"`
	Generated bool                 `json:"generated"`
	Covers    sessionSummaryCovers `json:"covers"`
	// Trace is the LINK to the ordered transcript — the record the summary
	// indexes. The trace is always one read underneath, never replaced.
	Trace string `json:"trace"`
	// Threads carries the per-thread generated summary cards, ordered by
	// each thread's first message seq (the same cards
	// GET /sessions/{id}/threads/{thread_id}/summary serves one at a time).
	Threads []threadSummaryCard `json:"threads"`
}

// HandleSessionOutput serves GET /sessions/{id}/output?mode=trace|summary.
func (h *Handler) HandleSessionOutput(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	switch mode {
	case "", "trace":
		// Delegate, do not fork: the transcript payload (ordered, with
		// timestamps) is produced by the one read path that owns it.
		h.HandleTranscript(w, r)
		return
	case "summary":
	default:
		writeAPIError(w, http.StatusBadRequest, "OUTPUT_MODE_INVALID",
			fmt.Sprintf("mode %q is not one of trace|summary", mode))
		return
	}

	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}

	if len(st.Threads) == 0 {
		writeAPIError(w, http.StatusNotFound, "SUMMARY_UNAVAILABLE",
			fmt.Sprintf("session %q has no threads and therefore no generated summary; the trace at /sessions/%s/output?mode=trace is the record", sess.ID, sess.ID))
		return
	}

	cards := make([]threadSummaryCard, 0, len(st.Threads))
	ids := make([]string, 0, len(st.Threads))
	total := 0
	for _, t := range st.Threads {
		card := summaryCardOf(st, t, st.ThreadDepth(t.ID))
		total += card.MessageCount
		cards = append(cards, card)
		ids = append(ids, t.ID)
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].FirstSeq < cards[j].FirstSeq })

	writeJSON(w, http.StatusOK, sessionSummaryResponse{
		SessionID: sess.ID,
		Generated: true,
		Covers: sessionSummaryCovers{
			SessionID:    sess.ID,
			ThreadIDs:    ids,
			MessageCount: total,
		},
		Trace:   fmt.Sprintf("/sessions/%s/output?mode=trace", sess.ID),
		Threads: cards,
	})
}
