package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/crier-dev/crier/internal/registry"
)

// CR-FEAT-027. `crier keygen` is the whole reason this subcommand exists: the
// external review (DISPATCH · CRI-001, Carter) named the signing ceremony — an
// ed25519 keypair via `openssl genpkey`, the public half extracted with
// `openssl pkey -pubout -outform DER | tail -c 32 | xxd -p`, and a hand-written
// sig() shell helper to build X-Agent-Sig — as the number-one adoption killer,
// and it is the step the last three external/first-party testers each tripped
// over. None of that ceremony is a cryptographic requirement: it is only the
// absence of a first-party tool that speaks the wire format.
//
// keygen does the three things that ceremony was for, in-process, with no
// openssl and no xxd:
//
//  1. generate an ed25519 keypair (crypto/ed25519 + crypto/rand);
//  2. write the private key as a PKCS#8 PEM file, mode 0600 — byte-shaped
//     exactly like `openssl genpkey -algorithm ED25519 -out <file>`, so the
//     server's own loader (registry.LoadEd25519PrivateKeyFile, which the MCP
//     bridge uses through CRIER_AGENT_PRIVATE_KEY_FILE) reads it unchanged and
//     an operator who already has openssl can still read it with openssl;
//  3. PRINT the agent config — the agent id, the hex public key, and the exact
//     POST /agents body — plus the one command that finishes the job, so the
//     next step is a copy-paste rather than a DER-offset recipe.
//
// The printed public key is not taken on trust from the generation call: before
// anything is printed, the file is re-read from disk and parsed with the
// server's own loader, and a sample payload is signed with the parsed key and
// verified against the printed public key. If that self-test fails, keygen
// reports the failure and exits nonzero instead of printing a config the server
// would reject.
//
// This subcommand is additive and offline: it never starts a server and never
// talks to one (registration is the client libraries' one-line job), so it
// cannot be affected by — and cannot affect — a running deployment.

// keygenSelfTestPayload is the payload the self-test signs. It is never sent
// anywhere; only its signature is checked, and only against the key this
// invocation just wrote.
const keygenSelfTestPayload = "crier keygen self-test"

// keygenConfig is the machine-readable half of `crier keygen -json`: the same
// facts the human report prints, in the shape a script wants them (an
// orchestrator can hand "registration" straight to POST /agents).
type keygenConfig struct {
	AgentID      string `json:"agent_id"`
	PublicKey    string `json:"public_key"`
	KeyFile      string `json:"key_file"`
	Server       string `json:"server"`
	Registration string `json:"registration"`
}

// runKeygen implements `crier keygen [flags]`. It returns a process exit code
// and writes everything it has to say — the report and any failure — to out, so
// the whole surface is assertable from a test without touching os.Stderr.
func runKeygen(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("crier keygen", flag.ContinueOnError)
	fs.SetOutput(out)
	outFlag := fs.String("out", "crier-agent.key", "path of the private key file to write (PKCS#8 PEM ed25519, mode 0600)")
	idFlag := fs.String("id", "", "agent id to print in the registration config (default: the base name of -out without its extension)")
	serverFlag := fs.String("server", "http://localhost:8767", "server base URL printed in the client snippets")
	jsonFlag := fs.Bool("json", false, "print the agent config as a single JSON object instead of the human report")
	forceFlag := fs.Bool("force", false, "overwrite an existing key file (default: refuse — a keypair is not regenerable, and overwriting one strands every agent registered with its public key)")
	fs.Usage = func() { printKeygenUsage(out, fs) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/-help prints usage and exits 0, matching the server flags.
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(out, "crier keygen: unexpected argument %q — keygen takes flags only\n\n", fs.Arg(0))
		printKeygenUsage(out, fs)
		return 2
	}

	keyPath := *outFlag
	if keyPath == "" {
		fmt.Fprintln(out, "crier keygen: -out must not be empty")
		return 2
	}
	agentID := *idFlag
	if agentID == "" {
		agentID = defaultAgentID(keyPath)
	}
	if strings.TrimSpace(agentID) == "" {
		// The server's ONLY agent-id rule is PRESENCE (`POST /agents` answers
		// 400 "id is required" for an empty one), so this is that same rule
		// applied here, before a key is written for an identity the server
		// would refuse. Nothing stricter is imposed: any non-empty string is a
		// legal agent id, and inventing rules here would refuse ids the server
		// accepts.
		fmt.Fprintln(out, "crier keygen: agent id must not be empty (pass -id, or use a -out with a usable base name)")
		return 2
	}
	server := strings.TrimRight(*serverFlag, "/")

	// Refuse to clobber an existing path unless -force. A silent overwrite
	// would destroy the private half of a LIVE identity: agents already
	// registered with that public key could never sign again, and no
	// regeneration can undo it.
	if st, err := os.Stat(keyPath); err == nil {
		if st.IsDir() {
			fmt.Fprintf(out, "crier keygen: %s is a directory — -out must be a file path\n", keyPath)
			return 2
		}
		if !*forceFlag {
			fmt.Fprintf(out, "crier keygen: %s already exists — refusing to overwrite a private key.\n", keyPath)
			fmt.Fprintln(out, "  A keypair is not regenerable: an agent registered with its public key could never sign again.")
			fmt.Fprintln(out, "  Pick another -out, or pass -force to replace it deliberately.")
			return 2
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "crier keygen: cannot stat %s: %v\n", keyPath, err)
		return 1
	}

	// A nested -out (e.g. $HOME/.config/crier/agent.key) should just work: a
	// tester who has to mkdir first is back to ceremony. 0700 because the
	// parent of a private key should not be world-readable.
	if dir := filepath.Dir(keyPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintf(out, "crier keygen: cannot create %s: %v\n", dir, err)
			return 1
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(out, "crier keygen: key generation failed: %v\n", err)
		return 1
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		fmt.Fprintf(out, "crier keygen: cannot encode the private key as PKCS#8: %v\n", err)
		return 1
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// O_EXCL (unless -force) makes the refusal above race-free rather than a
	// check-then-write: two concurrent keygens on one path cannot both win.
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !*forceFlag {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(keyPath, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(out, "crier keygen: %s was created by someone else while keygen was running — refusing to overwrite it\n", keyPath)
			return 2
		}
		fmt.Fprintf(out, "crier keygen: cannot write %s: %v\n", keyPath, err)
		return 1
	}
	if _, err := f.Write(pemBytes); err != nil {
		f.Close()
		fmt.Fprintf(out, "crier keygen: cannot write %s: %v\n", keyPath, err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(out, "crier keygen: cannot write %s: %v\n", keyPath, err)
		return 1
	}
	// OpenFile's mode is masked by the process umask; chmod sets the mode the
	// report claims, and the mode is then read BACK from the filesystem and
	// printed from that reading rather than from this constant.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		fmt.Fprintf(out, "crier keygen: cannot set mode 0600 on %s: %v\n", keyPath, err)
		return 1
	}

	// Self-test. Everything printed below is derived from the file ON DISK,
	// parsed by the same function the server-side MCP bridge uses — not from
	// the private key still in memory.
	loaded, err := registry.LoadEd25519PrivateKeyFile(keyPath)
	if err != nil {
		fmt.Fprintf(out, "crier keygen: the file just written does not parse with the server's own key loader: %v\n", err)
		return 1
	}
	loadedPub, ok := loaded.Public().(ed25519.PublicKey)
	if !ok || !loadedPub.Equal(pub) {
		fmt.Fprintln(out, "crier keygen: self-test FAILED — the public key derived from the file on disk differs from the generated one")
		return 1
	}
	if !ed25519.Verify(loadedPub, []byte(keygenSelfTestPayload), ed25519.Sign(loaded, []byte(keygenSelfTestPayload))) {
		fmt.Fprintln(out, "crier keygen: self-test FAILED — a signature made with the file's key did not verify against its public key")
		return 1
	}
	st, err := os.Stat(keyPath)
	if err != nil {
		fmt.Fprintf(out, "crier keygen: cannot stat the file just written: %v\n", err)
		return 1
	}
	fileMode := st.Mode().Perm()

	pubHex := hex.EncodeToString(pub)
	registrationBytes, err := json.Marshal(map[string]string{"id": agentID, "public_key": pubHex})
	if err != nil {
		fmt.Fprintf(out, "crier keygen: cannot build the registration body: %v\n", err)
		return 1
	}
	registration := string(registrationBytes)

	if *jsonFlag {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(keygenConfig{
			AgentID:      agentID,
			PublicKey:    pubHex,
			KeyFile:      keyPath,
			Server:       server,
			Registration: registration,
		}); err != nil {
			fmt.Fprintf(out, "crier keygen: cannot write the config: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(out, "crier keygen — ed25519 keypair for agent %q\n\n", agentID)
	fmt.Fprintf(out, "  key file          : %s  (PKCS#8 PEM, mode %04o)\n", keyPath, fileMode)
	fmt.Fprintf(out, "  agent id          : %s\n", agentID)
	fmt.Fprintf(out, "  public key (hex)  : %s\n", pubHex)
	fmt.Fprintf(out, "  server            : %s\n", server)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  verified          : re-read the file from disk, parsed it with the server's own key loader, and")
	fmt.Fprintln(out, "                      signed + verified a sample payload with it — PASS.")
	fmt.Fprintln(out, "                      The public key above is the file's key, and this is exactly what the server")
	fmt.Fprintln(out, "                      does to every signed request. No openssl and no xxd were used.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Registration — POST /agents takes exactly this body (the clients below send it for you):")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s\n", registration)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next, with no openssl and no hex surgery:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  1. start the server (any terminal):")
	fmt.Fprintln(out, "       make run")
	fmt.Fprintln(out, "  2. register + run a full signed round-trip")
	fmt.Fprintln(out, "     (register → deliver → retrieve → ack → publish → subscribe):")
	fmt.Fprintf(out, "       python3 clients/python/round_trip.py --server %s --id %s --key %s\n", server, agentID, keyPath)
	fmt.Fprintf(out, "       node clients/typescript/round-trip.ts --server %s --id %s --key %s\n", server, agentID, keyPath)
	fmt.Fprintln(out, "  3. or in your own code:")
	fmt.Fprintln(out, "       from crier_client import Crier")
	fmt.Fprintf(out, "       c = Crier(%q, agent_id=%q, key_path=%q)\n", server, agentID, keyPath)
	fmt.Fprintln(out, "       c.register()                     # POST /agents with the public key above")
	fmt.Fprintln(out, "       c.deliver(\"bob\", {\"hello\": \"world\"})  # bob needs no key of his own to RECEIVE")
	fmt.Fprintln(out, "       for m in c.retrieve(): ...       # signed retrieve; c.ack(m) when handled")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Keep the key file secret — the server only ever needs the public key.")
	return 0
}

// defaultAgentID derives the agent id from the key path: the base name with a
// key/PEM extension removed, so `crier keygen -out alice.key` prints "alice"
// and `-out ~/.config/crier/alice.pem` prints "alice" too. A path with no
// usable base name falls back to "agent", which is a legal agent id — the
// printed id is only a default for the registration body, and -id overrides it.
func defaultAgentID(keyPath string) string {
	base := filepath.Base(keyPath)
	for _, ext := range []string{".key", ".pem", ".priv", ".pem.key"} {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "agent"
	}
	return base
}

// printKeygenUsage prints the keygen usage screen. It is a function rather than
// an inline closure so `-h` and a bad argument print the same text.
func printKeygenUsage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(out, "crier keygen — generate an ed25519 keypair and print the agent config (no openssl, no xxd)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  crier keygen [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	fs.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  crier keygen -out alice.key -id alice          # write alice.key (mode 0600) + print the config")
	fmt.Fprintln(out, "  crier keygen -out bob.key -server http://box:8767")
	fmt.Fprintln(out, "  crier keygen -out alice.key -json              # the same config as one JSON object")
}
