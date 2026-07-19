package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestMCPServerInitialize is an entrypoint smoke test: it builds and starts
// crier-mcp as a subprocess, sends a JSON-RPC initialize request over stdin,
// and verifies the response carries a result with serverInfo. If someone
// breaks the wiring in main.go (config load, store init, mcp.New, Serve),
// this test fails immediately.
func TestMCPServerInitialize(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crier-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build crier-mcp: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"CR_AUTH_TOKEN=",
		"CR_LOG_LEVEL=error",
		// Force the in-memory store regardless of the dev env.
		"CR_DATABASE_URL=",
		"DATABASE_URL=",
		"CRIER_DATABASE_URL=",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start crier-mcp: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n"
	if _, err := stdin.Write([]byte(initReq)); err != nil {
		t.Fatalf("write initialize request: %v", err)
	}

	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		r := bufio.NewReader(stdout)
		line, err := r.ReadBytes('\n')
		readCh <- readResult{line, err}
	}()

	var line []byte
	select {
	case res := <-readCh:
		if res.err != nil {
			t.Fatalf("read initialize response: %v", res.err)
		}
		line = res.line
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for initialize response")
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		Result  *struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
		Error any `json:"error"`
		ID    int `json:"id"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal initialize response %q: %v", line, err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %s", line)
	}
	if resp.Result == nil {
		t.Fatalf("initialize response missing result: %s", line)
	}
	if resp.Result.ServerInfo.Name != "crier-mcp" {
		t.Fatalf("result.serverInfo.name: got %q, want %q", resp.Result.ServerInfo.Name, "crier-mcp")
	}
	if resp.Result.ServerInfo.Version == "" {
		t.Fatal("result.serverInfo.version is empty")
	}
	if resp.Result.ProtocolVersion == "" {
		t.Fatal("result.protocolVersion is empty")
	}

	// Close stdin; Serve's scan loop should terminate and the process exit 0.
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("crier-mcp exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crier-mcp did not exit after stdin close")
	}
}
