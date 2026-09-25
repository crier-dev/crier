package a2a

import (
	"strings"
	"testing"
)

// acceptedA2AKeys is the member list a rejection prints, in the order Config
// declares them. Pinned literally (not derived) so a rejection message that
// loses or reorders a key fails here.
const acceptedA2AKeys = `enabled`

// TestDecodeConfig_StrictMembership is the contract of the strict `a2a` decode
// (INT-A2A-001, specs/A2A-OPTION.md §4.2): a member the config does not declare
// is refused with the offending key AND the accepted keys named, while every
// declared member (and every input the surrounding decoder would accept) still
// decodes.
//
// The defect this guards is the webhook row's (DF-CRIER-150) in A2A clothing:
// without the scan, {"a2a":{"enabld":true}} would be dropped by encoding/json
// and the caller would silently get an agent that never opted in while
// believing it had.
func TestDecodeConfig_StrictMembership(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // "" = must be accepted
	}{
		{
			name:    "misnamed enabled at the root",
			raw:     `{"enabld":true}`,
			wantErr: `a2a: unknown field "enabld" (accepted: ` + acceptedA2AKeys + `)`,
		},
		{
			name:    "unknown key alongside the declared one",
			raw:     `{"enabled":true,"card":"x"}`,
			wantErr: `a2a: unknown field "card" (accepted: ` + acceptedA2AKeys + `)`,
		},
		{
			// The scan must be no stricter than the decode it guards: Go's
			// decoder matches field names case-insensitively, so a key written
			// in another case is the SAME member, not a typo.
			name: "declared member in another case",
			raw:  `{"ENABLED":true}`,
		},
		{
			name: "valid object, the declared member",
			raw:  `{"enabled":true}`,
		},
		{
			name: "empty object is a valid block",
			raw:  `{}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeConfig([]byte(tc.raw))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeConfig(%s) = error %v, want accepted", tc.raw, err)
				}
				if cfg == nil {
					t.Fatalf("DecodeConfig(%s) = nil config, want a decoded value", tc.raw)
				}
				return
			}
			if err == nil {
				t.Fatalf("DecodeConfig(%s) = %+v, nil error; want error %q", tc.raw, cfg, tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("DecodeConfig(%s) error = %q, want %q", tc.raw, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDecodeConfig_Values pins what the accepted shapes decode TO: the flag is
// the whole of today's block, and the later-key-wins rule is the decoder's
// (the strict scan must not turn a duplicate member into an error).
func TestDecodeConfig_Values(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantEnabled bool
	}{
		{name: "opted in", raw: `{"enabled":true}`, wantEnabled: true},
		{name: "explicitly off", raw: `{"enabled":false}`, wantEnabled: false},
		{name: "empty block is off — the zero value", raw: `{}`, wantEnabled: false},
		{name: "explicit null member is off", raw: `{"enabled":null}`, wantEnabled: false},
		{name: "later member wins", raw: `{"enabled":true,"enabled":false}`, wantEnabled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeConfig([]byte(tc.raw))
			if err != nil {
				t.Fatalf("DecodeConfig(%s): %v", tc.raw, err)
			}
			if cfg.Enabled != tc.wantEnabled {
				t.Fatalf("DecodeConfig(%s).Enabled = %v, want %v", tc.raw, cfg.Enabled, tc.wantEnabled)
			}
		})
	}
}

// TestDecodeConfig_NullAndNonObject pins the two ends the scan does not judge:
// an explicit JSON null means "no block" (nil, nil — the removal signal both
// registration paths read), while a value that is neither an object nor null is
// refused by the decoder's own type error rather than silently accepted.
func TestDecodeConfig_NullAndNonObject(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`null`))
	if err != nil {
		t.Fatalf("DecodeConfig(null) = %v, want (nil, nil)", err)
	}
	if cfg != nil {
		t.Fatalf("DecodeConfig(null) = %+v, want nil config", cfg)
	}

	for _, raw := range []string{`true`, `"enabled"`, `[1]`, `42`, ``, `{`} {
		got, err := DecodeConfig([]byte(raw))
		if err == nil {
			t.Fatalf("DecodeConfig(%q) = %+v, nil error; want a rejection", raw, got)
		}
		if !strings.HasPrefix(err.Error(), "a2a: ") {
			t.Fatalf("DecodeConfig(%q) error = %q, want it to name the a2a object", raw, err.Error())
		}
	}

	// Trailing data after the object is refused: two blocks in one member is
	// not a shape any caller can mean, and accepting it would silently drop the
	// second.
	if _, err := DecodeConfig([]byte(`{"enabled":true}{"enabled":false}`)); err == nil {
		t.Fatal("DecodeConfig with trailing data = nil error, want a rejection")
	}
}
