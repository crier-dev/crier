package a2a

import "fmt"

// IdempotencyInfo is a send-time observation, not an unlimited exactly-once
// promise. It lives in Task/Message metadata.crier.idempotency (DF-CRIER-390).
type IdempotencyInfo struct {
	Durability    string  `json:"durability"`
	WindowSeconds float64 `json:"window_seconds"`
	Replay        string  `json:"replay"`
	Description   string  `json:"description"`
}

// NewIdempotencyInfo consumes evidence from the actual replay store and the
// effective window. The registry package is intentionally not imported here.
func NewIdempotencyInfo(durable bool, windowSeconds float64, replayed bool) *IdempotencyInfo {
	info := &IdempotencyInfo{Durability: "process", WindowSeconds: windowSeconds, Replay: "new"}
	limit := "Idempotency holds for the life of the process only; a restart may mint a second task for the same messageId."
	if durable {
		info.Durability = "durable"
		limit = "Idempotency survives process restart while the registry replay store is retained."
	}
	if replayed {
		info.Replay = "replayed"
	}
	info.Description = fmt.Sprintf("%s The messageId replay window is %g seconds from the first reservation; after expiry the same messageId may create a new task.", limit, windowSeconds)
	return info
}
