package webhook

import (
	"strings"
	"testing"
)

// acceptedWebhookKeys is the member list a rejection prints, in the order
// Config declares them. Pinned literally (not derived) so a rejection message
// that loses or reorders a key fails here.
const acceptedWebhookKeys = `url, auth_type, auth_value_ref, schema_template, custom_schema, delivery_mode, batch, retries, timeout_ms`

// TestDecodeConfig_StrictMembership is the contract of the strict webhook
// decode (DF-CRIER-150): a member the config does not declare is refused with
// the offending key AND the accepted keys named — at every nesting level —
// while every declared member (and every input the surrounding decoder would
// accept) still decodes.
func TestDecodeConfig_StrictMembership(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // "" = must be accepted
	}{
		{
			// The defect this row exists for: "mode" was dropped silently and
			// the caller silently got the async default.
			name:    "misnamed delivery_mode at the root",
			raw:     `{"url":"http://127.0.0.1:18971/hook","mode":"blocking"}`,
			wantErr: `webhook: unknown field "mode" (accepted: ` + acceptedWebhookKeys + `)`,
		},
		{
			name:    "unknown key at the root alongside valid ones",
			raw:     `{"url":"http://127.0.0.1:18971/hook","delivery_mode":"blocking","retries":3,"surprise":true}`,
			wantErr: `webhook: unknown field "surprise" (accepted: ` + acceptedWebhookKeys + `)`,
		},
		{
			name:    "unknown key nested in batch",
			raw:     `{"url":"http://127.0.0.1:18971/hook","delivery_mode":"batch","batch":{"max_msgs":5}}`,
			wantErr: `webhook.batch: unknown field "max_msgs" (accepted: max_messages, flush_interval_s)`,
		},
		{
			name:    "unknown key nested in custom_schema",
			raw:     `{"url":"http://127.0.0.1:18971/hook","custom_schema":{"response_maps":"choices.0"}}`,
			wantErr: `webhook.custom_schema: unknown field "response_maps" (accepted: request_shape, response_map)`,
		},
		{
			name:    "unknown key nested in custom_schema.request_shape",
			raw:     `{"url":"http://127.0.0.1:18971/hook","custom_schema":{"request_shape":{"bodd":{}}}}`,
			wantErr: `webhook.custom_schema.request_shape: unknown field "bodd" (accepted: method, headers, body)`,
		},
		{
			name: "valid object, every declared member",
			raw: `{"url":"http://127.0.0.1:18971/hook","auth_type":"bearer","auth_value_ref":"env:HOOK_TOKEN",` +
				`"schema_template":"openai-compatible","delivery_mode":"batch","retries":3,"timeout_ms":2500,` +
				`"batch":{"max_messages":10,"flush_interval_s":5},` +
				`"custom_schema":{"request_shape":{"method":"POST","headers":{"X-T":"1"},"body":{"m":"{{payload}}"}},"response_map":"choices.0.message.content"}}`,
		},
		{
			// The scan must be no stricter than the decode it guards: Go's
			// decoder matches field names case-insensitively, so a key written
			// in another case still resolves to a declared field.
			name: "declared key in a different case is still accepted",
			raw:  `{"URL":"http://127.0.0.1:18971/hook","Delivery_Mode":"blocking"}`,
		},
		{
			name: "empty object",
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
					t.Fatalf("DecodeConfig(%s) = nil config with nil error", tc.raw)
				}
				return
			}
			if err == nil {
				t.Fatalf("DecodeConfig(%s) accepted an undeclared member (cfg=%+v), want error %q", tc.raw, cfg, tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			if cfg != nil {
				t.Errorf("cfg = %+v, want nil alongside the error", cfg)
			}
		})
	}
}

// TestDecodeConfig_Values pins that the strict decode populates the config it
// accepts — a rejection-only test would pass on a decoder that returns an
// empty config for valid input.
func TestDecodeConfig_Values(t *testing.T) {
	raw := `{"url":"http://127.0.0.1:18971/hook","auth_type":"bearer","auth_value_ref":"env:HOOK_TOKEN",` +
		`"schema_template":"hermes-http-gateway","delivery_mode":"batch","retries":4,"timeout_ms":1500,` +
		`"batch":{"max_messages":7,"flush_interval_s":2},` +
		`"custom_schema":{"request_shape":{"method":"PUT","headers":{"X-A":"b"},"body":{"q":"{{payload.x}}"}},"response_map":"raw"}}`
	cfg, err := DecodeConfig([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if cfg.DeliveryMode != "batch" || cfg.Retries != 4 || cfg.TimeoutMs != 1500 {
		t.Errorf("scalars: %+v", cfg)
	}
	if cfg.Batch == nil || cfg.Batch.MaxMessages != 7 || cfg.Batch.FlushIntervalS != 2 {
		t.Errorf("batch: %+v", cfg.Batch)
	}
	if cfg.CustomSchema == nil || cfg.CustomSchema.RequestShape == nil {
		t.Fatalf("custom_schema: %+v", cfg.CustomSchema)
	}
	if cfg.CustomSchema.ResponseMap != "raw" || cfg.CustomSchema.RequestShape.Method != "PUT" {
		t.Errorf("custom_schema shape: %+v", cfg.CustomSchema)
	}
	if got := string(cfg.CustomSchema.RequestShape.Body); got != `{"q":"{{payload.x}}"}` {
		t.Errorf("request_shape body = %s", got)
	}
	// The decoded config must satisfy the pre-existing registration contract.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() on a decoded config: %v", err)
	}
}

// TestDecodeConfig_NullAndNonObject: an explicit null is the caller's removal
// signal (both registration paths read it that way), and a value that is
// neither object nor null is refused rather than silently treated as empty.
func TestDecodeConfig_NullAndNonObject(t *testing.T) {
	cfg, err := DecodeConfig([]byte("  null "))
	if err != nil {
		t.Fatalf("DecodeConfig(null) = %v, want (nil, nil)", err)
	}
	if cfg != nil {
		t.Fatalf("DecodeConfig(null) = %+v, want nil", cfg)
	}

	for _, raw := range []string{`"http://127.0.0.1:18971/hook"`, `[1,2]`, `17`, `{"url":`, `true`} {
		cfg, err := DecodeConfig([]byte(raw))
		if err == nil {
			t.Errorf("DecodeConfig(%s) = %+v, want an error", raw, cfg)
		}
	}

	// Trailing data after the object is not silently ignored either.
	if _, err := DecodeConfig([]byte(`{"url":"http://x/hook"} {"url":"http://y/hook"}`)); err == nil {
		t.Error("DecodeConfig accepted trailing data after the webhook object")
	} else if !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("trailing-data error = %v", err)
	}
}
