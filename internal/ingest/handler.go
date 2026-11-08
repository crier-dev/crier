package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/ratelimit"
	"github.com/crier-dev/crier/internal/session"
	"github.com/gorilla/mux"
)

const DefaultMaxBodyBytes = 65536

var sourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type InboxWriter interface {
	Deliver(ctx context.Context, agentID, capability string, msg ingress.InboxMessage) ([]session.DeliveryOutcome, error)
}

type Handler struct {
	Store         *Store
	Inbox         InboxWriter
	ReplayWindow  time.Duration
	MaxBodyBytes  int
	RatePerMinute int
	Now           func() time.Time
	NewID         func() (string, error)
	limiter       *ratelimit.Window
}

func (h *Handler) init() {
	if h.limiter == nil {
		h.limiter = ratelimit.NewWindow(time.Minute)
	}
}
func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.init()
	switch {
	case r.URL.Path == "/ingress/sources" && r.Method == http.MethodPost:
		h.createSource(w, r)
	case r.URL.Path == "/ingress/sources" && r.Method == http.MethodGet:
		h.listSources(w, r)
	case strings.HasPrefix(r.URL.Path, "/ingress/webhooks/") && r.Method == http.MethodPost:
		h.ingest(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

type sourceRequest struct {
	ID        string   `json:"id"`
	Mapping   string   `json:"mapping"`
	DeliverTo string   `json:"deliver_to"`
	Tags      []string `json:"tags"`
}

func (h *Handler) createSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if !sourceIDPattern.MatchString(req.ID) {
		writeJSON(w, 400, map[string]string{"error": "invalid source id"})
		return
	}
	if req.Mapping == "" {
		req.Mapping = "passthrough"
	}
	if req.Mapping != "passthrough" {
		writeJSON(w, 400, map[string]string{"error": "unknown mapping"})
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		writeJSON(w, 500, map[string]string{"error": "secret generation failed"})
		return
	}
	source := Source{ID: req.ID, Secret: secret, Mapping: req.Mapping, DeliverTo: req.DeliverTo, Tags: req.Tags, CreatedAt: h.now()}
	if err := h.Store.CreateSource(r.Context(), source); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeJSON(w, 409, map[string]string{"error": "source already exists"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "source store failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source_id": source.ID, "secret": hex.EncodeToString(secret), "created_at": source.CreatedAt})
}
func (h *Handler) listSources(w http.ResponseWriter, r *http.Request) {
	sources, err := h.Store.Sources(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "source store failed"})
		return
	}
	writeJSON(w, 200, sources)
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/ingress/webhooks/")
	if !sourceIDPattern.MatchString(id) {
		writeJSON(w, 404, map[string]string{"error": "unknown source"})
		return
	}
	source, err := h.Store.Source(r.Context(), id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSON(w, 404, map[string]string{"error": "unknown source"})
		} else {
			writeJSON(w, 500, map[string]string{"error": "source store failed"})
		}
		return
	}
	max := h.MaxBodyBytes
	if max <= 0 {
		max = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(max)))
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeJSON(w, 413, map[string]string{"error": "body too large"})
		} else {
			writeJSON(w, 400, map[string]string{"error": "invalid body"})
		}
		return
	}
	if r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" || r.Header.Get("Content-Type") != "application/json" || !validJSONNoDuplicateKeys(body) {
		writeJSON(w, 400, map[string]string{"error": "ambiguous or invalid JSON representation"})
		return
	}
	ts := r.Header.Get("X-Crier-Timestamp")
	sec, e := strconv.ParseInt(ts, 10, 64)
	if e != nil || ts == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid timestamp"})
		return
	}
	window := h.ReplayWindow
	if window <= 0 {
		window = 300 * time.Second
	}
	delta := h.now().Sub(time.Unix(sec, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > window {
		writeJSON(w, 409, map[string]string{"error": "timestamp outside replay window"})
		return
	}
	nonce := r.Header.Get("X-Crier-Nonce")
	nonceBytes, nonceErr := base64.RawURLEncoding.DecodeString(nonce)
	if nonceErr != nil || len(nonceBytes) < 16 {
		writeJSON(w, 400, map[string]string{"error": "invalid nonce"})
		return
	}
	sourceHeader := r.Header.Get("X-Crier-Source-ID")
	if sourceHeader == "" || sourceHeader != id {
		writeJSON(w, 401, map[string]string{"error": "signature verification failed"})
		return
	}
	sigHeader := r.Header.Get("X-Crier-Signature")
	provided, err := hex.DecodeString(strings.TrimPrefix(sigHeader, "sha256="))
	if err != nil || !strings.HasPrefix(sigHeader, "sha256=") {
		writeJSON(w, 400, map[string]string{"error": "invalid signature"})
		return
	}
	bodyHash := sha256.Sum256(body)
	canonicalPath := "/ingress/webhooks/" + id
	input := fmt.Sprintf("POST\n%s\n%s\n%s\n%s\n%x", canonicalPath, sourceHeader, ts, nonce, bodyHash)
	mac := hmac.New(sha256.New, source.Secret)
	_, _ = mac.Write([]byte(input))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		writeJSON(w, 401, map[string]string{"error": "signature verification failed"})
		return
	}
	if !h.limiter.Allow(id, h.RatePerMinuteOrDefault(), time.Minute) {
		writeJSON(w, 429, map[string]string{"error": "source rate exceeded"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = hex.EncodeToString(bodyHash[:])
	}
	messageID, err := h.newID()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "message id generation failed"})
		return
	}
	duplicate, acceptedID, err := h.Store.Accept(r.Context(), id, key, nonce, messageID, body, h.now())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "durable acceptance failed"})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"duplicate": true, "message_id": acceptedID})
		return
	}
	if err := h.deliver(r.Context(), source, acceptedID, key, body); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "message delivery pending"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message_id": acceptedID})
}

func (h *Handler) deliver(ctx context.Context, source Source, messageID, key string, body []byte) error {
	if h.Inbox == nil {
		return fmt.Errorf("inbox writer is unavailable")
	}
	_, err := h.Inbox.Deliver(ctx, source.DeliverTo, "", ingress.InboxMessage{
		Author: session.AuthorRef{System: "webhook/" + source.ID},
		Kind:   session.MessageKind("text"), Text: "webhook event from " + source.ID,
		Payload: json.RawMessage(body), Tags: source.Tags, IdempotencyKey: key,
	})
	if err != nil {
		return err
	}
	return h.Store.MarkDelivered(ctx, source.ID, key, messageID)
}

// Drain retries every durable outbox row still awaiting inbox delivery.
func (h *Handler) Drain(ctx context.Context) error {
	rows, err := h.Store.PendingRows(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if err := h.deliver(ctx, row.Source, row.MessageID, row.Key, row.Payload); err != nil {
			failures = append(failures, fmt.Errorf("deliver %s: %w", row.MessageID, err))
		}
	}
	return errors.Join(failures...)
}
func (h *Handler) RatePerMinuteOrDefault() int {
	if h.RatePerMinute <= 0 {
		return 60
	}
	return h.RatePerMinute
}
func (h *Handler) newID() (string, error) {
	if h.NewID != nil {
		return h.NewID()
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func validJSONNoDuplicateKeys(raw []byte) bool {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if !jsonValue(d) {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
}
func jsonValue(d *json.Decoder) bool {
	tok, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return false
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return false
			}
			seen[key] = true
			if !jsonValue(d) {
				return false
			}
		}
		end, e := d.Token()
		return e == nil && end == json.Delim('}')
	case '[':
		for d.More() {
			if !jsonValue(d) {
				return false
			}
		}
		end, e := d.Token()
		return e == nil && end == json.Delim(']')
	default:
		return false
	}
}

var _ = mux.Vars
