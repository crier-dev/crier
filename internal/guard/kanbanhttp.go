package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// HTTPKanbanWriter writes cards by POSTing the card JSON to a kanban sink
// (CR-FEAT-009) — the CR_GUARD_KANBAN_URL integration point noted on
// HermesKanbanWriter. The sink contract: POST <base URL> with
// Content-Type application/json and the card JSON (§8.2) as the body; any
// 2xx status is success, anything else (or a network failure / ctx
// deadline) is an error. The per-card budget comes from ctx — the guard
// worker applies kanbanWriteTimeout (10s, spec §8.2) — and failures are
// fire-and-forget like the CLI writer: logged + counted by the worker,
// never surfaced to the delivery path (§8.1).
type HTTPKanbanWriter struct {
	baseURL string
	client  *http.Client
}

// NewHTTPKanbanWriter builds an HTTP-backed CardWriter. baseURL must be an
// absolute http(s) URL — the sink receives the POST verbatim. Returns nil
// for any other scheme or an unparseable/empty URL: the caller fails fast
// at startup (config validation, spec §9.1) instead of discovering the
// misconfiguration on the first blocked message.
func NewHTTPKanbanWriter(baseURL string) *HTTPKanbanWriter {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	return &HTTPKanbanWriter{baseURL: baseURL, client: &http.Client{}}
}

// WriteCard POSTs the card JSON to the sink. 2xx = success; non-2xx,
// network errors, and ctx deadlines → error (wrapped with context). The
// response body is drained so keep-alive connections return to the pool.
func (w *HTTPKanbanWriter) WriteCard(ctx context.Context, c Card) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("kanban: marshal card: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("kanban: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("kanban: post card: %w", err)
	}
	defer resp.Body.Close()
	resBody, _ := io.ReadAll(io.LimitReader(resp.Body, kanbanResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("kanban: sink returned %s", resp.Status)
		if len(resBody) > 0 {
			msg += ": " + truncateBody(resBody)
		}
		return errors.New(msg)
	}
	return nil
}

// kanbanResponseBytes bounds how much of a sink response is read (body
// drain + error excerpt) per card.
const kanbanResponseBytes = 512
