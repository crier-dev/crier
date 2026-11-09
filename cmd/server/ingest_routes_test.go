package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWebhookIngestRoutesAreAbsentWhenGateOff(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN": "ingress-deployment-token",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	for _, path := range []string{"/ingress/webhooks/build-bot", "/ingress/sources"} {
		resp, body := webhookRouteRequest(t, client, http.MethodPost, base+path, "", nil, nil)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "CR_INGEST_WEBHOOK_ENABLED") {
			t.Fatalf("POST %s status=%d body=%s; want 404 naming CR_INGEST_WEBHOOK_ENABLED", path, resp.StatusCode, body)
		}
	}
}

func TestWebhookIngestFreshStoreMigrationAndRouteAuthPosture(t *testing.T) {
	parallelBoot(t)
	token := "ingress-deployment-token"
	storePath := t.TempDir() + "/ingest.sqlite"
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":             token,
		"CR_REQUIRE_AGENT_SIG":      "false",
		"CR_INGEST_WEBHOOK_ENABLED": "true",
		"CR_INGEST_STORE_FILE":      storePath,
		"CR_SESSION_LOG_ROOT":       t.TempDir(),
		"CR_GROUP_ROOT":             t.TempDir(),
	})
	client := &http.Client{Timeout: 5 * time.Second}

	// A fresh store must be migrated by the production boot path before the
	// real ingest handler can register a source.
	registerAgent := `{"id":"webhook-target"}`
	resp, body := webhookRouteRequest(t, client, http.MethodPost, base+"/agents", token, []byte(registerAgent), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register target status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = webhookRouteRequest(t, client, http.MethodPost, base+"/ingress/sources", "", []byte(`{"id":"build-bot","deliver_to":"webhook-target"}`), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Authorization") {
		t.Fatalf("source registration without bearer status=%d body=%s; want bearer 401", resp.StatusCode, body)
	}

	resp, body = webhookRouteRequest(t, client, http.MethodPost, base+"/ingress/sources", token, []byte(`{"id":"build-bot","deliver_to":"webhook-target"}`), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("source registration status=%d body=%s", resp.StatusCode, body)
	}
	var source struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(body, &source); err != nil || source.Secret == "" {
		t.Fatalf("source registration response secret: err=%v body=%s", err, body)
	}
	secret, err := hex.DecodeString(source.Secret)
	if err != nil {
		t.Fatalf("decode source secret: %v", err)
	}

	resp, body = webhookRouteRequest(t, client, http.MethodPost, base+"/ingress/webhooks/build-bot", "", []byte(`{"action":"build"}`), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "source authentication required") {
		t.Fatalf("webhook without source auth status=%d body=%s; want source authentication 401", resp.StatusCode, body)
	}

	payload := []byte(`{"action":"build"}`)
	now := time.Now().Unix()
	nonce := base64.RawURLEncoding.EncodeToString([]byte("fresh-store-nonce-0001"))
	hash := sha256.Sum256(payload)
	transcript := fmt.Sprintf("POST\n/ingress/webhooks/build-bot\nbuild-bot\n%d\n%s\n%x", now, nonce, hash)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(transcript))
	headers := map[string]string{
		"Content-Type":      "application/json",
		"X-Crier-Source-ID": "build-bot",
		"X-Crier-Timestamp": fmt.Sprint(now),
		"X-Crier-Nonce":     nonce,
		"X-Crier-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		"Idempotency-Key":   "fresh-store-event-1",
	}
	resp, body = webhookRouteRequest(t, client, http.MethodPost, base+"/ingress/webhooks/build-bot", "", payload, headers)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("signed webhook status=%d body=%s; want 202", resp.StatusCode, body)
	}
}

func webhookRouteRequest(t *testing.T, client *http.Client, method, url, token string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, url, err)
	}
	return resp, responseBody
}
