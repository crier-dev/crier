package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

// CR-FEAT-027: `crier keygen` replaces the openssl + xxd + hand-written sig()
// ceremony the external review named as the number-one adoption killer. These
// tests pin the properties that make it a replacement rather than a wrapper: the
// file it writes is the PKCS#8 PEM the SERVER's own loader reads, its mode is
// 0600, the public key in the printed config is the key in the file (not a
// second derivation), the self-test runs before anything is printed, and an
// existing private key is never destroyed by accident.

// keygenTempPath returns a path inside a fresh per-test directory.
func keygenTempPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func TestKeygenWritesAServerReadablePKCS8KeyAtMode0600(t *testing.T) {
	keyPath := keygenTempPath(t, "alice.key")
	var out bytes.Buffer

	if rc := runKeygen([]string{"-out", keyPath, "-id", "alice", "-server", "http://box:9000"}, &out); rc != 0 {
		t.Fatalf("runKeygen returned %d, want 0\noutput:\n%s", rc, out.String())
	}

	report := out.String()
	pubHex := keygenReportValue(t, report, "public key (hex)  : ")
	if len(pubHex) != ed25519.PublicKeySize*2 {
		t.Fatalf("printed public key is %d hex chars, want %d", len(pubHex), ed25519.PublicKeySize*2)
	}

	// The file must parse with the loader the server-side MCP bridge uses
	// (CRIER_AGENT_PRIVATE_KEY_FILE) — the whole point of writing PKCS#8 PEM.
	priv, err := registry.LoadEd25519PrivateKeyFile(keyPath)
	if err != nil {
		t.Fatalf("the file keygen wrote does not load with the server's own loader: %v", err)
	}
	derived, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("loaded key's public half is %T, want ed25519.PublicKey", priv.Public())
	}
	if hex.EncodeToString(derived) != pubHex {
		t.Fatalf("the printed public key is not the file's key: printed %s, file %s", pubHex, hex.EncodeToString(derived))
	}

	// The PEM block type is what openssl would have written, so an operator who
	// already has openssl can still read the file with it.
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the key file: %v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("the key file is not a PKCS#8 'PRIVATE KEY' PEM block (got %v)", block)
	}

	// Mode 0600: the report CLAIMS it, so the filesystem is what is checked.
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat the key file: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file mode is %04o, want 0600", got)
	}
	if !strings.Contains(report, "0600") {
		t.Errorf("the report does not state the mode it measured:\n%s", report)
	}

	// The report is the config: the exact POST /agents body, the id, and the
	// client snippets a non-crypto tester follows next.
	if !strings.Contains(report, `{"id":"alice","public_key":"`+pubHex+`"}`) {
		t.Errorf("the report does not carry the exact registration body:\n%s", report)
	}
	if !strings.Contains(report, "verified") || !strings.Contains(report, "PASS") {
		t.Errorf("the report does not state the self-test result:\n%s", report)
	}
	if !strings.Contains(report, "python3 clients/python/round_trip.py") || !strings.Contains(report, "node clients/typescript/round-trip.ts") {
		t.Errorf("the report does not point at both first-party clients:\n%s", report)
	}
	if strings.Contains(report, "openssl pkeyutl") {
		t.Errorf("the report still leans on an openssl incantation:\n%s", report)
	}
}

// TestKeygenSelfTestProvesThePrintedKeySigns: the signature the self-test makes
// over keygenSelfTestPayload must verify against the PRINTED public key — the
// same relationship the server checks on every signed request.
func TestKeygenSelfTestProvesThePrintedKeySigns(t *testing.T) {
	keyPath := keygenTempPath(t, "bob.key")
	var out bytes.Buffer
	if rc := runKeygen([]string{"-out", keyPath, "-id", "bob"}, &out); rc != 0 {
		t.Fatalf("runKeygen returned %d, want 0", rc)
	}
	pubHex := keygenReportValue(t, out.String(), "public key (hex)  : ")
	pub, err := hex.DecodeString(pubHex)
	if err != nil {
		t.Fatalf("printed public key is not hex: %v", err)
	}
	priv, err := registry.LoadEd25519PrivateKeyFile(keyPath)
	if err != nil {
		t.Fatalf("load the key: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(keygenSelfTestPayload))
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(keygenSelfTestPayload), sig) {
		t.Fatal("a signature from the key file does not verify against the printed public key")
	}
}

func TestKeygenRefusesToOverwriteAnExistingKey(t *testing.T) {
	keyPath := keygenTempPath(t, "carol.key")
	var first bytes.Buffer
	if rc := runKeygen([]string{"-out", keyPath, "-id", "carol"}, &first); rc != 0 {
		t.Fatalf("first keygen returned %d, want 0", rc)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the key: %v", err)
	}

	var second bytes.Buffer
	rc := runKeygen([]string{"-out", keyPath, "-id", "carol"}, &second)
	if rc == 0 {
		t.Fatal("a second keygen over an existing key succeeded, want a refusal")
	}
	if rc != 2 {
		t.Errorf("refusal exit code is %d, want 2 (usage-class)", rc)
	}
	if !strings.Contains(second.String(), "refusing to overwrite") || !strings.Contains(second.String(), "-force") {
		t.Errorf("the refusal does not name the cause and the escape hatch:\n%s", second.String())
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the key after the refusal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the refused run modified the existing private key")
	}

	// -force is the deliberate replacement, and it must actually replace.
	var forced bytes.Buffer
	if rc := runKeygen([]string{"-out", keyPath, "-id", "carol", "-force"}, &forced); rc != 0 {
		t.Fatalf("runKeygen -force returned %d, want 0\n%s", rc, forced.String())
	}
	replaced, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the key after -force: %v", err)
	}
	if bytes.Equal(before, replaced) {
		t.Fatal("-force left the old key in place")
	}
}

func TestKeygenRefusesToWriteOverADirectory(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if rc := runKeygen([]string{"-out", dir}, &out); rc != 2 {
		t.Fatalf("runKeygen -out <dir> returned %d, want 2\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "is a directory") {
		t.Errorf("the refusal does not name the problem:\n%s", out.String())
	}
}

// TestKeygenCreatesNestedParentDirectories: a tester who wants the key under
// ~/.config/crier must not be sent back to mkdir — that is ceremony again.
func TestKeygenCreatesNestedParentDirectories(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "nested", "deeper", "dana.key")
	var out bytes.Buffer
	if rc := runKeygen([]string{"-out", keyPath}, &out); rc != 0 {
		t.Fatalf("runKeygen returned %d, want 0\n%s", rc, out.String())
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("the key was not written to the nested path: %v", err)
	}
	// The default agent id comes from the file's base name.
	if !strings.Contains(out.String(), `ed25519 keypair for agent "dana"`) {
		t.Errorf("the default agent id was not derived from the path:\n%s", out.String())
	}
}

func TestKeygenJSONIsTheMachineReadableHalf(t *testing.T) {
	keyPath := keygenTempPath(t, "erin.key")
	var out bytes.Buffer
	if rc := runKeygen([]string{"-out", keyPath, "-id", "erin", "-server", "http://box:9000/", "-json"}, &out); rc != 0 {
		t.Fatalf("runKeygen -json returned %d, want 0\n%s", rc, out.String())
	}
	var cfg keygenConfig
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		t.Fatalf("the -json output is not JSON: %v\n%s", err, out.String())
	}
	if cfg.AgentID != "erin" || cfg.KeyFile != keyPath {
		t.Errorf("config names the wrong identity/file: %+v", cfg)
	}
	if cfg.Server != "http://box:9000" {
		t.Errorf("the server URL kept a trailing slash: %q", cfg.Server)
	}
	if len(cfg.PublicKey) != ed25519.PublicKeySize*2 {
		t.Errorf("config public key is %d hex chars: %q", len(cfg.PublicKey), cfg.PublicKey)
	}
	if cfg.Registration != `{"id":"erin","public_key":"`+cfg.PublicKey+`"}` {
		t.Errorf("the registration body is not the exact POST /agents body: %q", cfg.Registration)
	}
}

func TestKeygenRejectsBadArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"extra argument", []string{"extra"}, "unexpected argument"},
		{"empty -out", []string{"-out", ""}, "-out must not be empty"},
		{"empty agent id", []string{"-out", keygenTempPath(t, "x.key"), "-id", "   "}, "agent id must not be empty"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if rc := runKeygen(tc.args, &out); rc == 0 {
				t.Fatalf("runKeygen(%v) returned 0, want a refusal", tc.args)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output does not name %q:\n%s", tc.want, out.String())
			}
		})
	}
}

func TestKeygenHelpExitsZeroAndNamesTheRules(t *testing.T) {
	var out bytes.Buffer
	if rc := runKeygen([]string{"-h"}, &out); rc != 0 {
		t.Fatalf("keygen -h returned %d, want 0", rc)
	}
	for _, want := range []string{"crier keygen", "-out", "-id", "-server", "-json", "-force", "no openssl"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("keygen usage is missing %q:\n%s", want, out.String())
		}
	}
}

// TestRunDispatchesKeygenWithoutStartingAServer: the subcommand is handled
// BEFORE flag parsing and before any listener is bound, so `crier keygen` can
// never fall through into a running server (and every other argument shape keeps
// the pre-existing flag behaviour).
func TestRunDispatchesKeygenWithoutStartingAServer(t *testing.T) {
	keyPath := keygenTempPath(t, "frank.key")
	if rc := run([]string{"keygen", "-out", keyPath, "-id", "frank"}); rc != 0 {
		t.Fatalf("run([keygen …]) returned %d, want 0", rc)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("run([keygen …]) did not write the key: %v", err)
	}
	if _, err := registry.LoadEd25519PrivateKeyFile(keyPath); err != nil {
		t.Fatalf("the key run([keygen …]) wrote does not load: %v", err)
	}
}

// TestKeygenIsNotAServerFlag: the pre-existing flags are untouched — `keygen` is
// dispatched as a subcommand, and a bare/unknown first argument must still be
// handled by the flag parser (an unknown flag exit code stays 2).
func TestKeygenIsNotAServerFlag(t *testing.T) {
	var out bytes.Buffer
	if _, _, _, _, _, _, err := parseArgs([]string{"-nope"}, &out); err == nil {
		t.Fatal("parseArgs accepted an unknown flag")
	}
	var usage bytes.Buffer
	help, _, _, _, _, _, err := parseArgs([]string{"-h"}, &usage)
	if err != nil || !help {
		t.Fatalf("parseArgs(-h) = (help=%v, err=%v), want (true, nil)", help, err)
	}
	if !strings.Contains(usage.String(), "crier keygen [flags]") {
		t.Errorf("the server usage screen does not advertise the keygen subcommand:\n%s", usage.String())
	}
}

// keygenReportValue pulls one "  <label>: <value>" line out of the human report.
func keygenReportValue(t *testing.T, report, label string) string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "  "+label) {
			return strings.TrimSpace(strings.TrimPrefix(line, "  "+label))
		}
	}
	t.Fatalf("the report has no %q line:\n%s", label, report)
	return ""
}
