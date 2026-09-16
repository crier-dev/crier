package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// truncateRuneSafe bounds s to maxBytes WITHOUT splitting a multi-byte
// UTF-8 rune (DF-CRIER-186). When len(s) <= maxBytes s is returned
// unchanged (ASCII output is therefore byte-identical to a naive
// s[:maxBytes]). Otherwise the cut backs off from maxBytes to the start
// of the last COMPLETE rune whose bytes fit, so the result is always
// valid UTF-8.
func truncateRuneSafe(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Render projects a payload into the guard's user-message payload section
// (spec §6). JSON payloads get a schema-aware text projection (§6.1);
// non-JSON payloads get a text envelope (§6.2); empty payloads get the
// <empty_payload/> marker. Output is bounded by maxBytes (default 32768).
// The projection is pure: same payload → same text, always.
func Render(payload []byte, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = 32768
	}
	if len(payload) == 0 {
		return "<empty_payload/>", nil
	}
	if json.Valid(payload) {
		var w strings.Builder
		budget := maxBytes
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		renderJSONValue(dec, &w, "root", 0, &budget)
		out := "<json_projection>\n" + w.String() + "</json_projection>"
		if budget <= 0 {
			out += fmt.Sprintf("\n... (projection truncated at %d bytes)", maxBytes)
		}
		return out, nil
	}
	// Non-JSON payload: opaque text envelope (§6.2).
	n := len(payload)
	trunc := ""
	if n > maxBytes {
		payload = []byte(truncateRuneSafe(string(payload), maxBytes)) // rune-safe cut (DF-CRIER-186)
		trunc = "\n…(truncated)"
	}
	return fmt.Sprintf("<text_envelope length=%d>\n%s%s\n</text_envelope>", n, string(payload), trunc), nil
}

// maxDepth caps JSON nesting at 8 levels (spec §6.1: deeper levels emit
// path=<…truncated…> and stop).
const maxDepth = 8

// renderJSONValue writes one JSON value's projection at path. Returns
// false when the byte budget is exhausted (caller stops and the marker is
// appended by Render).
func renderJSONValue(d *json.Decoder, w *strings.Builder, path string, depth int, budget *int) bool {
	tok, err := d.Token()
	if err != nil {
		return true // end of input — treat as done
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			if !emitLine(w, path+"=object", budget) {
				return false
			}
			if depth >= maxDepth {
				emitLine(w, path+"=<…truncated…>", budget)
				skipContainer(d)
				return true
			}
			for d.More() {
				keyTok, err := d.Token()
				if err != nil {
					return true
				}
				key, _ := keyTok.(string)
				if !renderJSONValue(d, w, path+"."+key, depth+1, budget) {
					return false
				}
			}
			if _, err := d.Token(); err != nil { // consume closing }
				return true
			}
			return true
		case '[':
			if !emitLine(w, path+"=array", budget) {
				return false
			}
			if depth >= maxDepth {
				emitLine(w, path+"=<…truncated…>", budget)
				skipContainer(d)
				return true
			}
			idx := 0
			for d.More() {
				if !renderJSONValue(d, w, fmt.Sprintf("%s[%d]", path, idx), depth+1, budget) {
					return false
				}
				idx++
			}
			if _, err := d.Token(); err != nil { // consume closing ]
				return true
			}
			return true
		}
	default:
		return emitLeaf(w, path, tok, budget)
	}
	return true
}

// skipContainer consumes tokens until the container whose opening delim
// was already read closes (depth-cap truncation, spec §6.1).
func skipContainer(d *json.Decoder) {
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			return
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
}

// emitLeaf writes a scalar leaf: path=type value (spec §6.1).
func emitLeaf(w *strings.Builder, path string, tok any, budget *int) bool {
	var line string
	switch t := tok.(type) {
	case string:
		line = fmt.Sprintf("%s=string %s", path, quoteValue(t))
	case json.Number:
		line = fmt.Sprintf("%s=number %s", path, t.String())
	case bool:
		line = fmt.Sprintf("%s=bool %v", path, t)
	case nil:
		line = fmt.Sprintf("%s=null", path)
	default:
		line = fmt.Sprintf("%s=%v", path, tok)
	}
	return emitLine(w, line, budget)
}

// quoteValue escapes a string value and rune-truncates at 500 with a …
// suffix (spec §6.1). json.Marshal escapes quotes, backslashes and control
// chars so the projection cannot smuggle unescaped delimiters.
func quoteValue(s string) string {
	const maxRunes = 500
	r := []rune(s)
	suffix := ""
	if len(r) > maxRunes {
		r = r[:maxRunes]
		suffix = "…"
	}
	esc, _ := json.Marshal(string(r))
	return string(esc) + suffix
}

// emitLine writes one projection line, accounting for the byte budget.
func emitLine(w *strings.Builder, line string, budget *int) bool {
	n := len(line) + 1 // + newline
	if n > *budget {
		// Partial tail line so the truncation is visible, then stop.
		rest := *budget
		if rest > 8 {
			rest = 8
		}
		// Rune-safe partial cut (DF-CRIER-186): a 3-byte rune straddling
		// the tail boundary must never be split. If the cut would land
		// mid-rune (backed off to a shorter, still-rune-safe prefix) the
		// visible tail is dropped rather than show a broken rune.
		partial := truncateRuneSafe(line, rest)
		if len(partial) != rest {
			partial = ""
		}
		w.WriteString(partial)
		w.WriteString("\n")
		*budget = 0
		return false
	}
	w.WriteString(line)
	w.WriteString("\n")
	*budget -= n
	return true
}
