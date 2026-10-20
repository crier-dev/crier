package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Read the producer's declarations, not a hand-maintained test list. A newly
// named basis MUST get its own sentence even if no UI fixture exercises it yet.
func TestChatTrustBasisVocabulary(t *testing.T) {
	files, err := filepath.Glob("../../internal/trust/*.go")
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string]bool{}
	for _, filename := range files {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			for _, spec := range group.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if !strings.HasPrefix(name.Name, "TrustBasis") {
						continue
					}
					if i >= len(value.Values) {
						t.Fatalf("%s: expected an explicit wire value", name.Name)
					}
					literal, ok := value.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatalf("%s: expected a string wire value", name.Name)
					}
					basis, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					bases[basis] = true
				}
			}
		}
	}
	if len(bases) == 0 {
		t.Fatal("no server trust bases found")
	}
	seen := map[string]string{}
	for basis := range bases {
		sentence := chatTrustBasisWords[basis]
		if strings.TrimSpace(sentence) == "" || !strings.HasSuffix(sentence, ".") {
			t.Errorf("%s has no explanation sentence", basis)
		}
		if other, ok := seen[sentence]; ok {
			t.Errorf("%s and %s share an explanation", basis, other)
		}
		seen[sentence] = basis
	}
	for basis := range chatTrustBasisWords {
		if !bases[basis] {
			t.Errorf("panel explains %s, which is not a server basis", basis)
		}
	}
}

func TestChatTrustBasisServedMap(t *testing.T) {
	resp, err := http.Get(startTestServer(t) + "/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /chat: %d", resp.StatusCode)
	}
	match := regexp.MustCompile(`const basisWords = (\{[^\n]*\});`).FindSubmatch(body)
	if match == nil {
		t.Fatal("served script has no rendered basis map")
	}
	var got map[string]string
	if err := json.Unmarshal(match[1], &got); err != nil {
		t.Fatal(err)
	}
	for basis, want := range chatTrustBasisWords {
		if got[basis] != want {
			t.Errorf("%s: rendered %q; want %q (absent entry previously rendered No basis explanation reported)", basis, got[basis], want)
		}
	}
	if !reflect.DeepEqual(got, chatTrustBasisWords) {
		t.Errorf("rendered map differs from Go-side table: got %d entries, want %d", len(got), len(chatTrustBasisWords))
	}
}

func TestChatTrustBasisRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to execute the basis renderer")
	}
	source := string(chatClientHTML)
	start := strings.Index(source, "const basisWords = ")
	if start < 0 {
		t.Fatal("basis map not found")
	}
	end := strings.Index(source[start:], "$(\"management-chain\").append(explanation);")
	if end < 0 {
		t.Fatal("basis renderer not found")
	}
	renderer := source[start : start+end]
	words, err := json.Marshal(chatTrustBasisWords)
	if err != nil {
		t.Fatal(err)
	}
	script := `const assert = require('node:assert/strict');
const document = {createElement: () => ({textContent:''})};
function render(body) {
` + renderer + `
return explanation.textContent;
}
const words = ` + string(words) + `;
for (const [basis, sentence] of Object.entries(words)) {
  const actual = render({basis});
  assert.equal(actual, 'Basis: ' + basis + ' — ' + sentence, basis);
  console.log(actual);
}
// Future vocabulary is server evidence, NOT a missing explanation. Include
// inherited object-property names and markup to catch prototype/XSS shortcuts.
for (const basis of ['proof-future-check', 'constructor', '__proto__', '<img src=x onerror=alert(1)>', '  server string  ']) {
  assert.equal(render({basis}), 'Basis: ' + basis);
}
for (const body of [{}, {basis:''}, {basis:null}]) {
  assert.equal(render(body), 'Basis: not reported by server');
}
`
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("basis renderer: %v\n%s", err, out)
	}
	t.Logf("rendered full Go-side vocabulary:\n%s", out)
}
