package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/registry"
)

type Archiver interface {
	Archive(context.Context, Record) (Result, error)
}

type Record struct {
	Key     string `json:"key"`
	Domain  string `json:"domain"`
	Content string `json:"content"`
}

type Result struct {
	Key        string
	ArchivedAt time.Time
}

type Config struct {
	BaseURL      string
	Namespace    string
	KeyFile      string
	MaxBodyBytes int
}

type DuckBrainArchiver struct {
	cfg    Config
	client *http.Client
}

var attempts = metrics.Default.NewCounterVec("crier_archive_attempts_total", "DuckBrain archive attempts by outcome.", "outcome")
var duration = metrics.Default.NewHistogram("crier_archive_duration_seconds", "DuckBrain archive request duration in seconds.", metrics.DefaultBuckets)

func NewDuckBrainArchiver(cfg Config, client *http.Client) *DuckBrainArchiver {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "crier"
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 96000
	}
	return &DuckBrainArchiver{cfg: cfg, client: client}
}

func ValidateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return fmt.Errorf("CR_DUCKBRAIN_URL is required when CR_ARCHIVE_ENABLED=true")
	}
	if strings.TrimSpace(cfg.KeyFile) == "" {
		return fmt.Errorf("CR_DUCKBRAIN_API_KEY_FILE is required when CR_ARCHIVE_ENABLED=true")
	}
	st, err := os.Stat(cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("CR_DUCKBRAIN_API_KEY_FILE: %w", err)
	}
	if st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("CR_DUCKBRAIN_API_KEY_FILE must not be group/world accessible (mode 0600 required)")
	}
	return nil
}

func (a *DuckBrainArchiver) Archive(ctx context.Context, rec Record) (Result, error) {
	started := time.Now()
	defer func() { duration.Observe(time.Since(started).Seconds()) }()
	key, err := os.ReadFile(a.cfg.KeyFile)
	if err != nil {
		attempts.With("failed").Inc()
		return Result{}, fmt.Errorf("read DuckBrain API key file: %w", err)
	}
	key = bytes.TrimSpace(key)
	if len(key) == 0 {
		attempts.With("failed").Inc()
		return Result{}, fmt.Errorf("DuckBrain API key file is empty")
	}
	body, err := json.Marshal(map[string]any{"key": rec.Key, "domain": rec.Domain, "content": rec.Content, "attributes": map[string]any{}})
	if err != nil {
		attempts.With("failed").Inc()
		return Result{}, fmt.Errorf("marshal DuckBrain archive: %w", err)
	}
	if len(body) > a.cfg.MaxBodyBytes {
		attempts.With("too_large").Inc()
		return Result{}, fmt.Errorf("archive body exceeds configured maximum")
	}
	base := strings.TrimRight(a.cfg.BaseURL, "/")
	u, err := url.Parse(base + "/api/memories")
	if err != nil {
		attempts.With("failed").Inc()
		return Result{}, fmt.Errorf("parse DuckBrain URL: %w", err)
	}
	q := u.Query()
	q.Set("namespace", a.cfg.Namespace)
	u.RawQuery = q.Encode()
	var lastErr error
	for i, wait := range []time.Duration{0, 2 * time.Second, 8 * time.Second} {
		if i > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				lastErr = ctx.Err()
				i = 3
			case <-timer.C:
			}
		}
		if i > 2 {
			break
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if reqErr != nil {
			lastErr = reqErr
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", string(key))
		resp, reqErr := a.client.Do(req)
		if reqErr != nil {
			lastErr = reqErr
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			attempts.With("archived").Inc()
			return Result{Key: rec.Key, ArchivedAt: time.Now().UTC()}, nil
		}
		lastErr = fmt.Errorf("DuckBrain returned HTTP %d", resp.StatusCode)
	}
	attempts.With("failed").Inc()
	return Result{}, lastErr
}

// ArchiveDeadLetter encodes and archives one registry record. Oversized payloads
// are refused without truncation and counted separately.
func ArchiveDeadLetter(ctx context.Context, arch Archiver, dl *registry.DeadLetter, maxPayload int) bool {
	if dl == nil || arch == nil {
		return false
	}
	if maxPayload > 0 && len(dl.Payload) > maxPayload {
		attempts.With("too_large").Inc()
		return false
	}
	content, err := json.Marshal(dl)
	if err != nil {
		attempts.With("failed").Inc()
		return false
	}
	key := "crier/dead-letter/" + EscapeSegment(dl.AgentID) + "/" + EscapeSegment(dl.MessageID)
	_, err = arch.Archive(ctx, Record{Key: key, Domain: "crier", Content: string(content)})
	return err == nil
}

// EscapeSegment is exported to keep the deterministic key's path segments safe.
func EscapeSegment(s string) string { return url.PathEscape(s) }

// EndpointURL constructs a namespace-scoped DuckBrain API URL.
func EndpointURL(base, namespace string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/") + "/api/memories")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("namespace", namespace)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
