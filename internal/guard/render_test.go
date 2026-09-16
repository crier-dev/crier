package guard

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRender_JSONProjection(t *testing.T) {
	payload := []byte(`{"kind":"agent-request","data":{"text":"hello","count":3,"ok":true,"nil":null,"items":["a",{"role":"user","nested":{"deep":{"deeper":{"deepest":{"x":1}}}}}]}}`)
	out, err := Render(payload, 32768)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"<json_projection>",
		"root=object",
		"root.kind=string \"agent-request\"",
		"root.data=object",
		`root.data.text=string "hello"`,
		"root.data.count=number 3",
		"root.data.ok=bool true",
		"root.data.nil=null",
		"root.data.items=array",
		`root.data.items[0]=string "a"`,
		"root.data.items[1]=object",
		`root.data.items[1].role=string "user"`,
		"</json_projection>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("projection missing %q:\n%s", want, out)
		}
	}
}

func TestRender_StringTruncationAndEscapes(t *testing.T) {
	long := strings.Repeat("x", 600)
	payload := []byte(`{"a":"` + long + `","b":"say \"hi\"\nand \\back"}`)
	out, err := Render(payload, 32768)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `root.a=string "`+strings.Repeat("x", 500)+`"…`) {
		t.Errorf("string not rune-truncated at 500 with …:\n%s", out)
	}
	// Escaped quotes/backslashes must survive (no unescaped delimiters).
	if !strings.Contains(out, `root.b=string "say \"hi\"\nand \\back"`) {
		t.Errorf("escapes not preserved:\n%s", out)
	}
	// The raw 600-char value must not appear unescaped.
	if strings.Contains(out, strings.Repeat("x", 600)) {
		t.Errorf("untruncated value leaked")
	}
}

func TestRender_ByteCap(t *testing.T) {
	payload := []byte(`{"a":"` + strings.Repeat("y", 200) + `","b":"` + strings.Repeat("z", 200) + `"}`)
	out, err := Render(payload, 64)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "... (projection truncated at 64 bytes)") {
		t.Errorf("truncation marker missing:\n%s", out)
	}
}

func TestRender_DepthCap(t *testing.T) {
	payload := []byte(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":"deep"}}}}}}}}}`)
	out, err := Render(payload, 32768)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "<…truncated…>") {
		t.Errorf("depth cap marker missing:\n%s", out)
	}
}

func TestRender_TextEnvelope(t *testing.T) {
	payload := []byte("plain text payload, not json at all") // 35 bytes
	out, err := Render(payload, 32768)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(out, "<text_envelope length=35>") {
		t.Errorf("text envelope header missing: %q", out)
	}
	if !strings.Contains(out, "plain text payload, not json at all") {
		t.Errorf("text content missing")
	}
	if !strings.HasSuffix(out, "</text_envelope>") {
		t.Errorf("text envelope footer missing")
	}
}

func TestRender_TextEnvelopeTruncation(t *testing.T) {
	payload := []byte(strings.Repeat("z", 100))
	out, err := Render(payload, 32)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(out, "<text_envelope length=100>") {
		t.Errorf("length must be the ORIGINAL byte count: %q", out)
	}
	if !strings.Contains(out, "…(truncated)") {
		t.Errorf("truncation marker missing")
	}
}

func TestRender_EmptyPayload(t *testing.T) {
	out, err := Render(nil, 32768)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "<empty_payload/>" {
		t.Errorf("empty payload = %q, want <empty_payload/>", out)
	}
}

func TestRender_JSONPrimitives(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"just a string"`, `root=string "just a string"`},
		{`42`, `root=number 42`},
		{`true`, `root=bool true`},
		{`null`, `root=null`},
	} {
		out, err := Render([]byte(tc.in), 32768)
		if err != nil {
			t.Fatalf("Render(%s): %v", tc.in, err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("Render(%s) missing %q:\n%s", tc.in, tc.want, out)
		}
	}
}

// Determinism: same payload → same projection, always.
func TestRender_Deterministic(t *testing.T) {
	payload := []byte(`{"z":1,"a":{"m":[1,2,{"k":"v"}]},"m":"x"}`)
	a, _ := Render(payload, 32768)
	b, _ := Render(payload, 32768)
	if a != b {
		t.Fatalf("projection not deterministic:\n%s\nvs\n%s", a, b)
	}
}

// TestRender_TextEnvelopeRuneSafe (T5a): a non-JSON payload whose 3-byte
// rune straddles the cap must not split the rune — output stays valid
// UTF-8 and keeps the truncation marker (DF-CRIER-186). With the pre-fix
// `payload = payload[:maxBytes]` this fails: the cut lands mid-rune and
// utf8.ValidString is false (verified against the old line once).
func TestRender_TextEnvelopeRuneSafe(t *testing.T) {
	payload := []byte(strings.Repeat("a", 30) + "€" + strings.Repeat("b", 10)) // rune starts at byte 30, ends at 33
	out, err := Render(payload, 32)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Errorf("Render output is not valid UTF-8 — rune split mid-sequence:\n%q", out)
	}
	if !strings.Contains(out, "…(truncated)") {
		t.Errorf("truncation marker missing:\n%q", out)
	}
	// The cut backs off to a complete rune boundary: 30 a's, no partial €.
	if !strings.Contains(out, strings.Repeat("a", 30)) || strings.Contains(out, "aaa€") {
		t.Errorf("rune-safe backoff wrong:\n%q", out)
	}
}

// TestRender_TextEnvelopeASCIIByteIdentical (T5b): an ASCII payload's
// Render output is byte-identical to the pre-change behavior — the
// rune-safe helper is a no-op on pure ASCII (hard-coded expectation).
func TestRender_TextEnvelopeASCIIByteIdentical(t *testing.T) {
	payload := []byte("plain ascii text, exactly over budget here padding") // 50 bytes
	out, err := Render(payload, 40)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "<text_envelope length=50>\n" + string(payload[:40]) + "\n…(truncated)\n</text_envelope>"
	if out != want {
		t.Errorf("ASCII envelope changed:\n got %q\nwant %q", out, want)
	}
}
