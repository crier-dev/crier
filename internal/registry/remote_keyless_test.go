package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// DF-CRIER-198: a keyless agent (registered with signature enforcement off,
// public_key omitted → wire "public_key":"") must be READABLE through the
// RemoteStore. Before the fix, HexKey.UnmarshalJSON rejected the empty key,
// so Get errored and List silently swallowed the decode error and returned
// ZERO agents for the whole registry.

// TestRemoteStore_KeylessGet proves the Get read path: register a keyless
// agent through the real unsigned server, then Get must succeed and return
// an empty key.
func TestRemoteStore_KeylessGet(t *testing.T) {
	srv, _ := newRemoteTestServer(t) // signing disabled: keyless allowed
	rs := NewRemoteStore(srv.URL, "remote-client", "")

	if err := rs.Register(&Agent{ID: "keyless"}); err != nil {
		t.Fatalf("Register keyless: %v", err)
	}

	got, err := rs.Get("keyless")
	if err != nil {
		t.Fatalf("Get keyless: %v (want success — empty key is legal since DF-CRIER-192)", err)
	}
	if got.ID != "keyless" {
		t.Fatalf("Get = %+v", got)
	}
	if len(got.PublicKey) != 0 {
		t.Fatalf("keyless agent PublicKey = %x, want empty", []byte(got.PublicKey))
	}
}

// TestRemoteStore_ListIncludesKeyless pins the silent-empty-list regression:
// one keyed + one keyless agent, List must return BOTH. Before the fix a
// single keyless agent made List swallow the decode error and report 0.
func TestRemoteStore_ListIncludesKeyless(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL, "remote-client", "")

	keyBytes, err := hex.DecodeString(strings.Repeat("ab", ed25519.PublicKeySize))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if err := rs.Register(&Agent{ID: "keyed", PublicKey: HexKey(keyBytes), Capabilities: []string{"chat"}}); err != nil {
		t.Fatalf("Register keyed: %v", err)
	}
	if err := rs.Register(&Agent{ID: "keyless"}); err != nil {
		t.Fatalf("Register keyless: %v", err)
	}

	list := rs.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d agents, want 2: %+v", len(list), list)
	}
	seen := map[string]int{}
	for _, a := range list {
		seen[a.ID]++
		switch a.ID {
		case "keyed":
			if hex.EncodeToString([]byte(a.PublicKey)) != strings.Repeat("ab", ed25519.PublicKeySize) {
				t.Fatalf("keyed agent key = %x", []byte(a.PublicKey))
			}
		case "keyless":
			if len(a.PublicKey) != 0 {
				t.Fatalf("keyless agent key = %x, want empty", []byte(a.PublicKey))
			}
		}
	}
	if seen["keyed"] != 1 || seen["keyless"] != 1 {
		t.Fatalf("List ids = %v, want keyed and keyless exactly once each", seen)
	}
}
