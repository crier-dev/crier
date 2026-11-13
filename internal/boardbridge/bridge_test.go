package boardbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeBoardctl(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("LOG", filepath.Join(t.TempDir(), "argv.log"))
	t.Setenv("TIMING_LOG", filepath.Join(t.TempDir(), "timing.log"))
	path := filepath.Join(t.TempDir(), "boardctl")
	script := "#!/bin/sh\n: \"${TIMING_LOG:=$LOG}\"\nif [ \"$3\" = doctor ]; then exit \"${DOCTOR_EXIT:-0}\"; fi\nif [ \"$1 $2\" = 'version --json' ]; then printf '{}\\n'; exit 0; fi\nif [ \"$3\" = header ]; then if [ \"${INVALID_HEADER:-0}\" = 1 ]; then printf 'not-json\\n'; else printf '{}\\n'; fi; exit 0; fi\nprintf '%s\\n' \"$*\" >> \"$LOG\"\nprintf 'start %s\\n' \"$(date +%s%N)\" >> \"$TIMING_LOG\"\n" + body + "\nprintf 'end %s\\n' \"$(date +%s%N)\" >> \"$TIMING_LOG\"\n"
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
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("LOG", log)
	t.Setenv("PID_FILE", pidFile)
	bin := fakeBoardctl(t, `echo $$ > "$PID_FILE"; sleep 20; printf '{"ok":true}'`)
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
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid marker: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	waitProcessGroupDead(t, pid, 5*time.Second)
}

// waitProcessGroupDead distinguishes a live member from an unreaped zombie.
// kill(0) alone is insufficient here: Linux keeps a zombie's /proc entry and
// still reports the pid as existing until its parent reaps it.
func waitProcessGroupDead(t *testing.T, pgid int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		members, err := liveProcessGroupMembers(pgid)
		if err == nil && len(members) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	members, err := liveProcessGroupMembers(pgid)
	t.Fatalf("process group %d still has live members after timeout kill: members=%v stat_error=%v", pgid, members, err)
}

func liveProcessGroupMembers(pgid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var live []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		state, group, err := readProcStat(pid)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if group == pgid && state != 'Z' {
			live = append(live, pid)
		}
	}
	return live, nil
}

func readProcStat(pid int) (state byte, pgid int, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	close := strings.LastIndexByte(string(raw), ')')
	if close < 0 {
		return 0, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(raw)[close+1:])
	// After comm, fields[0] is state and fields[2] is pgrp (stat field 5).
	if len(fields) < 3 {
		return 0, 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, fmt.Errorf("pgrp for /proc/%d/stat: %w", pid, err)
	}
	return fields[0][0], group, nil
}

func TestReplyCatalogSummariesAndMutationNote(t *testing.T) {
	for _, tc := range []struct {
		name, raw, output, want string
	}{
		{"create", `{"v":1,"op":"create","args":{"id":"INT-101","title":"new"}}`, `{"id":"INT-101","status":"ready"}`, "Row INT-101 status ready … (uncommitted; board files commit via your board flow)"},
		{"update", `{"v":1,"op":"update","args":{"id":"INT-102","status":"blocked"}}`, `{"id":"INT-102","status":"blocked"}`, "Row INT-102 status blocked … (uncommitted; board files commit via your board flow)"},
		{"list", `{"v":1,"op":"list","args":{}}`, `{"rows":[{},{}]}`, "Listed 2 rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeBoardctl(t, `case "$3" in create) printf '`+tc.output+`' ;; update) printf '`+tc.output+`' ;; list) printf '`+tc.output+`' ;; *) printf '{"ok":true}' ;; esac`)
			cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
			bridge, err := New(cfg, bin)
			if err != nil {
				t.Fatal(err)
			}
			got, err := bridge.Deliver(context.Background(), cfg, "s", []byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != tc.want {
				t.Fatalf("text=%q want %q", got.Text, tc.want)
			}
			if string(got.Payload) != tc.output {
				t.Fatalf("payload=%s want verbatim %s", got.Payload, tc.output)
			}
		})
	}
}

func TestStartupDoctorFailureLogsAndHeaderFailureRefuses(t *testing.T) {
	bin := fakeBoardctl(t, `printf '{"ok":true}'`)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	t.Setenv("DOCTOR_EXIT", "3")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatalf("doctor failure blocked startup: %v", err)
	}
	if !strings.Contains(logs.String(), "boardctl doctor exit=3") {
		t.Fatalf("doctor log=%q", logs.String())
	}
	t.Setenv("INVALID_HEADER", "1")
	bridge, err = New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"show", "list"} {
		arg := `{"v":1,"op":"` + op + `","args":{"id":"INT-1"}}`
		if op == "list" {
			arg = `{"v":1,"op":"list","args":{}}`
		}
		_, err := bridge.Deliver(context.Background(), cfg, "s", []byte(arg))
		failure, ok := err.(*Error)
		want := "boardctl refused: board header unparsable at " + cfg.BoardDir
		if !ok || failure.BoardctlText != want {
			t.Fatalf("%s refusal=%#v want text %q", op, err, want)
		}
	}
	if data, readErr := os.ReadFile(os.Getenv("LOG")); readErr == nil && len(data) != 0 {
		t.Fatalf("delivery spawned boardctl: %s", data)
	}
}

func TestEmptyCommandAndPinnedBinaryLoss(t *testing.T) {
	bin := fakeBoardctl(t, `printf '{"ok":true}'`)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Deliver(context.Background(), cfg, "s", []byte(" \n\t"))
	if err == nil || err.Error() != "refused: empty command" {
		t.Fatalf("empty command error=%v", err)
	}
	if data, readErr := os.ReadFile(os.Getenv("LOG")); readErr == nil && len(data) != 0 {
		t.Fatalf("empty command spawned boardctl: %s", data)
	}
	if err := os.Chmod(bin, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Deliver(context.Background(), cfg, "s", []byte(`{"v":1,"op":"list","args":{}}`))
	failure, ok := err.(*Error)
	want := "pinned binary " + bin + " no longer executable"
	if !ok || failure.BoardctlText != want {
		t.Fatalf("binary-loss error=%#v want %q", err, want)
	}
}

func TestOutputTruncatesAt64KiBWithGuidance(t *testing.T) {
	bin := fakeBoardctl(t, `printf '{"rows":"'; head -c 70000 /dev/zero | tr '\000' x; printf '"}'`)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	got, err := bridge.Deliver(context.Background(), cfg, "s", []byte(`{"v":1,"op":"list","args":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Payload) > maxReplyBytes || !strings.Contains(string(got.Payload), "bytes truncated; use a narrower filter") {
		t.Fatalf("truncated payload len=%d tail=%q", len(got.Payload), got.Payload[len(got.Payload)-80:])
	}
}

func TestConcurrentDeliveriesSerializeWithNonOverlappingTimestamps(t *testing.T) {
	bin := fakeBoardctl(t, `sleep 0.12; printf '{"ok":true}'`)
	cfg := &Config{BoardDir: t.TempDir(), SenderAllowlist: []string{"s"}}
	bridge, err := New(cfg, bin)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"v":1,"op":"list","args":{}}`)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := bridge.Deliver(context.Background(), cfg, "s", payload); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(os.Getenv("TIMING_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	if len(lines) != 8 || lines[0] != "start" || lines[2] != "end" || lines[4] != "start" || lines[6] != "end" {
		t.Fatalf("timestamp markers=%q", data)
	}
	endFirst, err := strconv.ParseInt(lines[3], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	startSecond, err := strconv.ParseInt(lines[5], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if endFirst > startSecond {
		t.Fatalf("serialized spans overlap: end1=%d start2=%d", endFirst, startSecond)
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
