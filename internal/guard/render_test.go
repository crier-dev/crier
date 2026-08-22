package guard

import (
	"strings"
	"testing"
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
