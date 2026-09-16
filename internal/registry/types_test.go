package registry

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// TestHexKey_UnmarshalEmpty pins the keyless wire form (DF-CRIER-198): an
// empty key ("", zero decoded bytes) must decode to an EMPTY HexKey with no
// error — the same representation an in-memory or postgres keyless agent
// carries — so the RemoteStore read path can read such an agent.
func TestHexKey_UnmarshalEmpty(t *testing.T) {
	var k HexKey
	if err := json.Unmarshal([]byte(`""`), &k); err != nil {
		t.Fatalf("UnmarshalJSON(\"\"): %v (want nil — empty key is the keyless wire form)", err)
	}
	if len(k) != 0 {
		t.Fatalf("decoded key = %x, want empty", []byte(k))
	}
}

// TestHexKey_MarshalEmpty pins the marshal side of the keyless pairing
// (unchanged by DF-CRIER-198): an empty HexKey marshals to the empty string,
// which is what the handler writes for a keyless agent's public_key.
func TestHexKey_MarshalEmpty(t *testing.T) {
	b, err := json.Marshal(HexKey(nil))
	if err != nil {
		t.Fatalf("MarshalJSON(empty): %v", err)
	}
	if string(b) != `""` {
		t.Fatalf("MarshalJSON(empty) = %s, want \"\"", b)
	}
}

// TestHexKey_RoundTrip proves a real 32-byte key still marshals to its hex
// string and decodes back to identical bytes.
func TestHexKey_RoundTrip(t *testing.T) {
	raw, err := hex.DecodeString(strings.Repeat("ab", ed25519.PublicKeySize))
	if err != nil {
		t.Fatalf("decode fixture key: %v", err)
	}
	orig := HexKey(raw)

	wire, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if want := `"` + strings.Repeat("ab", ed25519.PublicKeySize) + `"`; string(wire) != want {
		t.Fatalf("MarshalJSON = %s, want %s", wire, want)
	}

	var got HexKey
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !bytes.Equal([]byte(got), raw) {
		t.Fatalf("round trip = %x, want %x", []byte(got), raw)
	}
}

// TestHexKey_UnmarshalRejectsBadLengths keeps the length guard strict except
// for the legal empty key: 31 and 33 bytes error with the existing wording.
func TestHexKey_UnmarshalRejectsBadLengths(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
		want string
	}{
		{"31 bytes", strings.Repeat("ab", 31), "invalid key length: 31, want 32"},
		{"33 bytes", strings.Repeat("ab", 33), "invalid key length: 33, want 32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var k HexKey
			err := json.Unmarshal([]byte(`"`+tc.hex+`"`), &k)
			if err == nil {
				t.Fatalf("decoded %s without error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestHexKey_UnmarshalRejectsNonHex keeps invalid hex failing with the
// existing wording: non-hex characters and an odd-length string.
func TestHexKey_UnmarshalRejectsNonHex(t *testing.T) {
	for _, tc := range []struct {
		name string
		hex  string
	}{
		{"non-hex characters", "zz" + strings.Repeat("a", 62)},
		{"odd-length hex", strings.Repeat("a", 63)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var k HexKey
			err := json.Unmarshal([]byte(`"`+tc.hex+`"`), &k)
			if err == nil {
				t.Fatalf("decoded %s without error", tc.name)
			}
			if !strings.Contains(err.Error(), "invalid hex key") {
				t.Fatalf("err = %v, want invalid hex key failure", err)
			}
		})
	}
}
