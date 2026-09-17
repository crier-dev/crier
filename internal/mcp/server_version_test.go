package mcp

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/registry"
)

// TestInitializeAdvertisesBuildinfoVersion is the DF-CRIER-171 acceptance proof
// for the MCP handshake: the initialize result must advertise the version
// segment of the ONE build identity, not a literal. Measured at HEAD before the
// fix: `./bin/crier-mcp --version` -> "crier-mcp v60cf425-dirty-60cf4251" while
// the same binary's initialize result answered serverInfo.version = "0.1.0".
//
// The claim is not merely "it equals some value" — it is that the advertised
// version and the identity `crier-mcp --version` prints are derivable from one
// another: the CLI string must carry the advertised segment verbatim.
func TestInitializeAdvertisesBuildinfoVersion(t *testing.T) {
	s := New(registry.NewMemoryStore())

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params:  mustMarshalJSON(t, initializeParams{ProtocolVersion: protocolVersion}),
		ID:      1,
	}
	resp := s.dispatch(t.Context(), req)
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %s", resp.Error.Message)
	}

	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal initialize result: %v", err)
	}
	var result initializeResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}

	want := buildinfo.VersionSegment()
	if result.ServerInfo.Version != want {
		t.Errorf("initialize serverInfo.version = %q, want %q (buildinfo.VersionSegment(), the segment `crier-mcp --version` prints)",
			result.ServerInfo.Version, want)
	}
	if identity := buildinfo.String(); !strings.Contains(identity, result.ServerInfo.Version) {
		t.Errorf("`crier-mcp --version` would print %q, which does not carry the advertised version segment %q — the two surfaces came apart",
			identity, result.ServerInfo.Version)
	}
}

// TestNoHardcodedMCPVersionLiteral pins the ABSENCE of the defect at the source
// level: the handshake version came from a hardcoded literal, so any semver
// literal reappearing in a shipping file of this package is the regression.
//
// It inspects STRING LITERALS through the AST, not raw file text, so prose in a
// comment may still name the old value ("the handshake used to answer 0.1.0")
// without tripping the guard. Test files are skipped on purpose: this asserts
// what ships in the binary, and a fixture may legitimately spell a version out.
func TestNoHardcodedMCPVersionLiteral(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	literal := regexp.MustCompile(`^"[0-9]+\.[0-9]+\.[0-9]+"$`)
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !literal.MatchString(lit.Value) {
				return true
			}
			t.Errorf("%s carries the hardcoded version literal %s — the MCP serverInfo.version and the mesh REGISTER capabilities must both come from internal/buildinfo (DF-CRIER-171)",
				fset.Position(lit.Pos()), lit.Value)
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test Go files — this guard is vacuous, fix the scanner before trusting it")
	}
}
