package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one chat-completions message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client is a minimal OpenAI-compatible chat-completions client for the
// guard (spec §5.3). It performs NO retries — the router owns
// retry/failover.
type Client struct {
	httpc    *http.Client
	baseURL  string
	apiKey   string
	model    string
	thinking bool
	timeout  time.Duration
}

// NewClient builds a guard LLM client. thinking adds the spec §3.2
// "thinking": {"type": "enabled"} field to every request; the deepseek
// preset forbids it (enforced at policy validation time, §5.2).
func NewClient(baseURL, apiKey, model string, thinking bool, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		httpc:    &http.Client{Timeout: timeout},
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		model:    model,
		thinking: thinking,
		timeout:  timeout,
	}
}

// chatRequest is the §3.2 request body: response_format json_object is
// ALWAYS sent (never downgraded), temperature 0, stream false, thinking
// only when enabled.
type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	Temperature    int            `json:"temperature"`
	ResponseFormat responseFormat `json:"response_format"`
	Stream         bool           `json:"stream"`
	Thinking       *thinkingField `json:"thinking,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type thinkingField struct {
	Type string `json:"type"`
}

// chatResponse is the subset of the chat-completions response the guard
// reads: choices[0].message.content as a string.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content any `json:"content"` // string; anything else = ErrProvider
		} `json:"message"`
	} `json:"choices"`
}

// Complete runs one chat completion and returns the raw assistant message
// content (spec §5.3). Errors: ErrProvider (HTTP/network/timeout/4xx-5xx),
// ErrModelRejected (400 on response_format or thinking fields — treated as
// provider failure by the router).
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	body := chatRequest{
		Model:          c.model,
		Messages:       messages,
		Temperature:    0,
		ResponseFormat: responseFormat{Type: "json_object"},
		Stream:         false,
	}
	if c.thinking {
		body.Thinking = &thinkingField{Type: "enabled"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("%w: marshal request: %v", ErrProvider, err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", ErrProvider, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusBadRequest {
		// 400 on response_format/thinking (or any other 400) — provider
		// failure, never a silent downgrade (spec §3.2).
		return "", fmt.Errorf("%w: status 400: %s", ErrModelRejected, truncateBody(respBody))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: status %d: %s", ErrProvider, resp.StatusCode, truncateBody(respBody))
	}

	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", fmt.Errorf("%w: parse response: %v", ErrProvider, err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("%w: no choices in response", ErrProvider)
	}
	content, ok := cr.Choices[0].Message.Content.(string)
	if !ok {
		return "", fmt.Errorf("%w: non-string message content", ErrProvider)
	}
	return content, nil
}

// truncateBody bounds error bodies for logs/reasons.
func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
