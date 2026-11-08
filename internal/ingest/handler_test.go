package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

type captureInbox struct{ messages []ingress.InboxMessage }

func (c *captureInbox) Deliver(_ context.Context, _ string, _ string, m ingress.InboxMessage) ([]session.DeliveryOutcome, error) {
	c.messages = append(c.messages, m)
	return []session.DeliveryOutcome{{InboxEntryID: fmt.Sprintf("entry-%d", len(c.messages))}}, nil
}

func setup(t *testing.T) (*Handler, *Store, *captureInbox, string, []byte) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "ingest.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(1762627200, 0).UTC()
	cap := &captureInbox{}
	h := &Handler{Store: store, Inbox: cap, ReplayWindow: 300 * time.Second, RatePerMinute: 60, Now: func() time.Time { return now }}
	req := httptest.NewRequest(http.MethodPost, "/ingress/sources", strings.NewReader(`{"id":"build-bot","mapping":"passthrough","tags":["webhook","ci"]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("register: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Secret) != 64 {
		t.Fatalf("secret length %d", len(result.Secret))
	}
	if strings.Count(rec.Body.String(), result.Secret) != 1 {
		t.Fatalf("registration response did not show the secret exactly once: %s", rec.Body.String())
	}
	secret, err := hex.DecodeString(result.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return h, store, cap, "build-bot", secret
}
func signedRequest(t *testing.T, h *Handler, id string, secret, body []byte, ts int64, nonce string) *httptest.ResponseRecorder {
	return signedRequestWithBody(t, h, id, secret, body, body, ts, nonce)
}
func signedRequestWithBody(t *testing.T, h *Handler, id string, secret, body, signedBody []byte, ts int64, nonce string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/ingress/webhooks/" + id
	hash := sha256.Sum256(signedBody)
	timestamp := fmt.Sprint(ts)
	input := fmt.Sprintf("POST\n%s\n%s\n%s\n%s\n%x", path, id, timestamp, nonce, hash)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(input))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Crier-Timestamp", timestamp)
	req.Header.Set("X-Crier-Nonce", nonce)
	req.Header.Set("X-Crier-Source-ID", id)
	req.Header.Set("X-Crier-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
func nonce(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("nonce-material-%04d", n)))
}

func TestSourceSecretIsShownOnceAndPayloadDelivered(t *testing.T) {
	h, _, cap, id, secret := setup(t)
	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/ingress/sources", nil))
	if list.Code != 200 || strings.Contains(list.Body.String(), hex.EncodeToString(secret)) {
		t.Fatalf("source list leaked secret or failed: %d %s", list.Code, list.Body.String())
	}
	body := []byte(`{"action":"build"}`)
	rec := signedRequest(t, h, id, secret, body, 1762627200, nonce(1))
	if rec.Code != 202 {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	if len(cap.messages) != 1 {
		t.Fatalf("delivered %d messages", len(cap.messages))
	}
	m := cap.messages[0]
	if m.Author.System != "webhook/"+id || m.Text != "webhook event from "+id || string(m.Payload) != string(body) || strings.Join(m.Tags, ",") != "webhook,ci" {
		t.Fatalf("bad mapped message: %#v", m)
	}
}
func TestSignatureReplayWindowAndPersistentDedup(t *testing.T) {
	h, store, cap, id, secret := setup(t)
	body := []byte(`{"run":1}`)
	tampered := signedRequestWithBody(t, h, id, secret, []byte(`{"run":2}`), []byte(`{"run":1}`), 1762627200, nonce(1))
	if tampered.Code != 401 {
		t.Fatalf("tampered body status %d", tampered.Code)
	}
	badKey := append([]byte(nil), secret...)
	badKey[0] ^= 0xff
	badSignature := signedRequest(t, h, id, badKey, body, 1762627200, nonce(8))
	if badSignature.Code != 401 {
		t.Fatalf("bad key status %d", badSignature.Code)
	}
	stale := signedRequest(t, h, id, secret, body, 1762626000, nonce(2))
	if stale.Code != 409 {
		t.Fatalf("stale timestamp status %d", stale.Code)
	}
	first := signedRequest(t, h, id, secret, body, 1762627200, nonce(3))
	if first.Code != 202 {
		t.Fatalf("first status %d %s", first.Code, first.Body.String())
	}
	path := store.path
	_ = store.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	h.Store = reopened
	again := signedRequest(t, h, id, secret, body, 1762627200, nonce(3))
	if again.Code != 200 {
		t.Fatalf("replay after reopen status=%d body=%s", again.Code, again.Body.String())
	}
	if len(cap.messages) != 1 {
		t.Fatalf("message count=%d", len(cap.messages))
	}
}
func TestOversizeAndDuplicateJSONRefusedBeforeHMAC(t *testing.T) {
	h, _, _, id, secret := setup(t)
	oversized := signedRequest(t, h, id, secret, []byte(`{"x":"`+strings.Repeat("a", DefaultMaxBodyBytes)+`"}`), 1762627200, nonce(4))
	if oversized.Code != 413 {
		t.Fatalf("oversize status=%d", oversized.Code)
	}
	dupe := signedRequest(t, h, id, secret, []byte(`{"x":1,"x":2}`), 1762627200, nonce(5))
	if dupe.Code != 400 {
		t.Fatalf("duplicate-key status=%d", dupe.Code)
	}
}
func TestSourceRateLimit61stRequest(t *testing.T) {
	h, _, _, id, secret := setup(t)
	h.RatePerMinute = 60
	for n := 1; n <= 61; n++ {
		rec := signedRequest(t, h, id, secret, []byte(fmt.Sprintf(`{"n":%d}`, n)), 1762627200, nonce(n+100))
		if n < 61 && rec.Code != 202 {
			t.Fatalf("request %d status=%d body=%s", n, rec.Code, rec.Body.String())
		}
		if n == 61 && rec.Code != 429 {
			t.Fatalf("61st request status=%d", rec.Code)
		}
	}
}
