package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeliverBlockingRequiresPerRunResponseToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Crier-Response-Token")
		if token == "" {
			t.Error("blocking request did not carry a response token")
		}
		w.Header().Set("X-Crier-Response-Token", token)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reply":"ok"}`))
	}))
	defer srv.Close()

	d := NewDriver(NewClient(time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())
	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", RequireResponseToken: true}
	reply, err := d.DeliverBlocking(t.Context(), "agent", cfg, &Envelope{Crier: EnvelopeMeta{MessageID: "m1"}}, time.Second)
	if err != nil {
		t.Fatalf("DeliverBlocking: %v", err)
	}
	if string(reply) != `{"reply":"ok"}` {
		t.Fatalf("reply = %s", reply)
	}
}

func TestDeliverBlockingRejectsMissingResponseToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reply":"unproven"}`))
	}))
	defer srv.Close()

	d := NewDriver(NewClient(time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())
	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", RequireResponseToken: true}
	_, err := d.DeliverBlocking(t.Context(), "agent", cfg, &Envelope{Crier: EnvelopeMeta{MessageID: "m2"}}, time.Second)
	if err == nil || err.Error() == "" {
		t.Fatal("missing response token must fail with a named error")
	}
	if !contains(err.Error(), "response token mismatch") {
		t.Fatalf("error = %v, want response token mismatch", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
