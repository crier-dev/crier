package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

func TestDuckBrainArchiveContractAndRefusal(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	var status int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/memories" || r.URL.Query().Get("namespace") != "cold" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("API key header missing")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Key     string `json:"key"`
			Domain  string `json:"domain"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.Key != "crier/dead-letter/agent-1/message-1" || got.Domain != "crier" {
			t.Errorf("unexpected archive record: %+v", got)
		}
		var dl registry.DeadLetter
		if err := json.Unmarshal([]byte(got.Content), &dl); err != nil {
			t.Fatal(err)
		}
		if dl.MessageID != "message-1" || dl.AgentID != "agent-1" {
			t.Errorf("content did not round trip: %+v", dl)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	arch := NewDuckBrainArchiver(Config{BaseURL: server.URL, Namespace: "cold", KeyFile: keyFile, MaxBodyBytes: 1000}, server.Client())
	dl := &registry.DeadLetter{MessageID: "message-1", AgentID: "agent-1", Payload: []byte("body")}
	status = http.StatusCreated
	if !ArchiveDeadLetter(context.Background(), arch, dl, 1000) {
		t.Fatal("2xx archive refused")
	}
	status = http.StatusInternalServerError
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	if ArchiveDeadLetter(context.Background(), arch, dl, 1000) {
		t.Fatal("500 archive accepted")
	}
	if bytes.Contains(logs.Bytes(), []byte("test-key")) {
		t.Fatal("API key value appeared in logs")
	}
}

func TestValidateConfigRejectsInsecureKeyMode(t *testing.T) {
	if err := ValidateConfig(Config{}); err == nil {
		t.Fatal("missing URL/key configuration accepted")
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("test-key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(Config{BaseURL: "http://example.invalid", KeyFile: keyFile}); err == nil {
		t.Fatal("permissive key file accepted")
	}
}
