package boardbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeBoardctl(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boardctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$LOG\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvelopeValidationAndArgv(t *testing.T) {
	cfg := &Config{BoardDir: "/board", SenderAllowlist: []string{"sender"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	good, err := Parse(cfg, []byte(`{"v":1,"op":"show","args":{"id":"INT-007"}}`))
	if err != nil {
		t.Fatal(err)
	}
	argv, err := Arguments(cfg, good)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-C", "/board", "show", "INT-007", "--json"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%q want %q", argv, want)
	}
	for _, raw := range []string{`{"v":2,"op":"show","args":{"id":"INT-007"}}`, `{"v":1,"op":"serve","args":{}}`, `{"v":1,"op":"show","args":{"id":"--output /tmp/exfil"}}`, `{"v":1,"op":"show","args":{"id":"INT-007","output":"/tmp/x"}}`} {
		if _, err := Parse(cfg, []byte(raw)); err == nil {
			t.Errorf("accepted unsafe envelope %s", raw)
		}
	}
}

func TestDeliveryACLAndVerbatimJSON(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	bin := fakeBoardctl(t, `printf '{"rows":[]}'`)
	t.Setenv("LOG", log)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"allowed"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"v":1,"op":"show","args":{"id":"INT-007"}}`)
	if _, err := bridge.Deliver(context.Background(), cfg, "blocked", payload); err == nil {
		t.Fatal("non-allowlisted sender accepted")
	}
	got, err := bridge.Deliver(context.Background(), cfg, "allowed", payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"rows":[]}` {
		t.Fatalf("payload=%s", got.Payload)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "-C "+cfg.BoardDir+" show INT-007 --json" {
		t.Fatalf("argv log=%q", data)
	}
}

func TestInvalidEnvelopeSpawnsNoSubprocess(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "counter")
	bin := fakeBoardctl(t, `echo called >> "$COUNT"; printf '{"ok":true}'`)
	t.Setenv("COUNT", counter)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Deliver(context.Background(), cfg, "s", []byte(`{"v":1,"op":"show","args":{"id":"--output /tmp/exfil"}}`))
	if err == nil {
		t.Fatal("unsafe envelope accepted")
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatalf("fake executable ran: stat=%v", err)
	}
}

func TestBoardctlRefusalTextIsPreserved(t *testing.T) {
	t.Setenv("LOG", filepath.Join(t.TempDir(), "argv.log"))
	bin := fakeBoardctl(t, `printf 'TR-251 evidence required' >&2; exit 7`)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Deliver(context.Background(), cfg, "s", []byte(`{"v":1,"op":"update","args":{"id":"INT-007","status":"complete"}}`))
	failure, ok := err.(*Error)
	if !ok || failure.ErrorKind != "refused" || failure.ExitCode != 7 || failure.BoardctlText != "TR-251 evidence required" {
		t.Fatalf("refusal=%#v", err)
	}
}

func TestConcurrentDeliveriesSerialize(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	bin := fakeBoardctl(t, `sleep 0.12; printf '{"ok":true}'`)
	t.Setenv("LOG", log)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	p := []byte(`{"v":1,"op":"list","args":{}}`)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := bridge.Deliver(context.Background(), cfg, "s", p); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 2 {
		t.Fatalf("calls=%q", data)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("LOG", log)
	bin := fakeBoardctl(t, `sleep 20; printf '{"ok":true}'`)
	cfg := &Config{BoardDir: t.TempDir(), TimeoutSecs: 5, SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = bridge.Deliver(context.Background(), cfg, "s", []byte(`{"v":1,"op":"list","args":{}}`))
	failure, ok := err.(*Error)
	if !ok || failure.ErrorKind != "timeout" {
		t.Fatalf("error=%#v", err)
	}
	if time.Since(start) > 7*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []*Config{{}, {BoardDir: "/b", TimeoutSecs: 4}, {BoardDir: "/b", AllowedOps: "show,unknown"}} {
		if err := c.Validate(); err == nil {
			t.Errorf("accepted invalid config: %#v", c)
		}
	}
	b, _ := json.Marshal(&Config{BoardDir: "/b"})
	if !strings.Contains(string(b), `"board_dir":"/b"`) {
		t.Fatalf("config JSON=%s", b)
	}
}
