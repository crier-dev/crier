package main

// INT-CI-023 — the child-boot budget, derived from a measurement instead of
// guessed, and the diagnostics a failed boot has to carry.
//
// BEFORE. Every spawned server child in this package carried its own hand-picked
// constant: a 20s pidfile handshake and a 20s readiness wait per attempt
// (main_test.go), a 40s -test.timeout and a 45s parent context for the registry
// child (registry_backend_test.go), 55s/60s for the a2a child
// (a2a_restart_test.go), and 5s per HTTP request. Independent numbers cannot be
// kept consistent, and these were not: the in-process retry envelope
// (3 attempts x 2 waits x 20s = 120s) is three times the 40s the child was
// allowed to live, so on a saturated host the child died of its own
// -test.timeout while it still had retry budget and the operator read a Go
// panic instead of a diagnosis.
//
// AFTER. One measurement (childBootMeasuredCost) scaled by the host's runnable
// queue per CPU produces the whole hierarchy at once:
//
//	Attempt  — one wait: the child publishes its pidfile, or /health answers
//	Attempts — the bounded retry count, named in every failure message
//	Envelope — Attempts * 2 * Attempt: the whole in-process retry envelope
//	ChildTimeout — Envelope + childBootInitSlack: the child's own -test.timeout
//	ParentCtx    — ChildTimeout + childBootInitSlack: the parent's context
//	Request      — the per-request budget inside a LIVE child
//
// Because they all come from one function, ChildTimeout > Envelope and
// ParentCtx > ChildTimeout hold at every load level: the hierarchy can no longer
// invert, which is what turned a slow boot into an unexplained panic.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/pidfile"
)

const (
	// childBootMeasuredCost is the MEASURED cold boot of one server child on
	// this host — exec of the coverage-instrumented test binary up to the child
	// writing its own pidfile. Measured 2026-11-03 at loadavg 23.75 (1m) /
	// 33.04 (5m) on 16 CPUs, n=8: min 193ms, median 425ms, max 512ms.
	//
	// The IN-PROCESS boot the pidfile wait actually governs is far cheaper
	// (n=20: min 5.1ms, median 5.3ms, max 23ms), but the parent waits for a
	// whole child process, so the child-process boot is the honest basis: a
	// budget derived from the 5ms figure would have no margin for exec, test
	// binary init and the coverage runtime.
	childBootMeasuredCost = 500 * time.Millisecond
	// childBootHeadroom multiplies the measurement into one attempt budget
	// (500ms x10 = 5s unloaded).
	childBootHeadroom = 10
	// childBootAttemptFloor is the idle-host margin: ~1000x the measured
	// in-process boot, so a healthy boot never approaches it, while a
	// genuinely stuck boot still fails in seconds instead of minutes.
	childBootAttemptFloor = 5 * time.Second
	// childBootAttemptCap bounds one attempt so a saturated host cannot stretch
	// a single wait without limit.
	childBootAttemptCap = 20 * time.Second
	// childBootAttempts is the bounded retry count. It is NAMED in every
	// failure message ("attempt 2/3") so a retry can never be mistaken for a
	// first try.
	childBootAttempts = 3
	// childBootInitSlack covers what the attempt budgets do NOT: exec,
	// test-binary init and the coverage runtime, paid once per process.
	childBootInitSlack = 15 * time.Second
	// childRequestFloor is the per-request budget inside a booted child. It is
	// today's 5s, floored so an idle host keeps exactly the old behaviour: the
	// recorded INT-CI-023 a2a failure was a POST that exceeded 5s at loadavg
	// 33.34 against a LIVE server — a request budget, not a boot budget.
	childRequestFloor = 5 * time.Second
	// childRequestCap bounds that growth.
	childRequestCap = 30 * time.Second
)

// childBootBudgetMarker is printed ONLY by a boot-budget failure. A parent
// harness (or an operator grepping a transcript) can therefore tell "the host
// starved the boot" from "the server refused to work", and a retry can never be
// built out of a real failure by accident.
const childBootBudgetMarker = "test-harness: server boot budget exhausted"

// errForeignChildPort marks a pidfile that describes a process this test did
// not spawn. It is NEVER retried: the whole point of the INT-CI-022 handshake is
// that the harness refuses to assert against a stranger, so a retry must not be
// able to turn that refusal into a pass.
var errForeignChildPort = errors.New("pidfile describes a foreign process")

// childBootPlan is this run's whole budget hierarchy, plus the inputs it was
// derived from so the failure messages can state their own conditions.
type childBootPlan struct {
	Load     float64
	CPUs     int
	Factor   float64
	Attempt  time.Duration
	Attempts int
	Envelope time.Duration
	Timeout  time.Duration
	Ctx      time.Duration
	Request  time.Duration
}

// planChildBoot derives the budget hierarchy from the measured cold boot and the
// host's current load, and LOGS the derivation: a failure then carries the
// numbers it was judged against instead of an unexplained constant.
func planChildBoot(t *testing.T) childBootPlan {
	t.Helper()
	load, haveLoad := readLoadAvg()
	plan := deriveChildBootPlan(load, haveLoad, runtime.NumCPU())
	t.Logf("child boot plan: attempt=%s attempts=%d envelope=%s child-timeout=%s parent-ctx=%s request=%s "+
		"(measured %s cold boot x%d, loadavg=%.2f over %d CPUs, factor %.2f, attempt floor %s, cap %s)",
		plan.Attempt, plan.Attempts, plan.Envelope, plan.Timeout, plan.Ctx, plan.Request,
		childBootMeasuredCost, childBootHeadroom, plan.Load, plan.CPUs, plan.Factor,
		childBootAttemptFloor, childBootAttemptCap)
	return plan
}

// deriveChildBootPlan is the pure derivation behind planChildBoot, kept separate
// so the arithmetic can be exercised without the log line.
func deriveChildBootPlan(load float64, haveLoad bool, cpus int) childBootPlan {
	factor := 1.0
	// A boot whose wall clock is mostly scheduling grows with the runnable
	// queue per CPU; at or below one queue per CPU the host is not contended.
	if haveLoad && load >= 0 && cpus > 0 && load > float64(cpus) {
		factor = load / float64(cpus)
	}
	attempt := time.Duration(float64(childBootMeasuredCost) * float64(childBootHeadroom) * factor)
	if attempt < childBootAttemptFloor {
		attempt = childBootAttemptFloor
	}
	if attempt > childBootAttemptCap {
		attempt = childBootAttemptCap
	}
	envelope := time.Duration(childBootAttempts) * 2 * attempt
	request := time.Duration(float64(childRequestFloor) * factor)
	if request < childRequestFloor {
		request = childRequestFloor
	}
	if request > childRequestCap {
		request = childRequestCap
	}
	return childBootPlan{
		Load:     load,
		CPUs:     cpus,
		Factor:   factor,
		Attempt:  attempt,
		Attempts: childBootAttempts,
		Envelope: envelope,
		Timeout:  envelope + childBootInitSlack,
		Ctx:      envelope + 2*childBootInitSlack,
		Request:  request,
	}
}

// readLoadAvg reads the 1-minute load average, the same field the a2a boot
// window has always used.
func readLoadAvg() (float64, bool) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return -1, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1, false
	}
	return load, true
}

// childLogDiagnosis turns the child's own log into the sentence an operator
// needs, by locating the last boot milestone the child reached. It is the
// difference between "never wrote its pidfile" (which says nothing) and "the
// child bound a port and stalled writing the pidfile, so this is the filesystem,
// not the server".
func childLogDiagnosis(logPath string) string {
	body, err := os.ReadFile(logPath)
	text := ""
	if err == nil {
		text = string(body)
	}
	reached := "the child logged no boot milestone at all: it has not reached run() (exec, test-binary init or config — not the listener)"
	switch {
	case strings.Contains(text, "pidfile written"):
		reached = "the child logged `pidfile written`, so the boot finished and the harness stalled reading the handshake — this is the filesystem, not the server"
	case strings.Contains(text, "ephemeral port bound"):
		reached = "the child logged `ephemeral port bound` but never `pidfile written`: it bound its port and stalled writing the pidfile"
	case strings.Contains(text, "crier starting"):
		reached = "the child logged `crier starting` but never bound a port: it is stuck inside run() before net.Listen (store or config init)"
	}
	return reached + "\nchild log tail:\n" + fileTail(logPath, 2048)
}

// childSpawnSpec is everything spawnServerChild needs. Every attempt gets its
// own Name, so the pidfile and the log of a failed attempt are never reused by
// the attempt that follows it.
type childSpawnSpec struct {
	Attempt    int
	Root       string
	Name       string
	RunPattern string
	Timeout    time.Duration
	Env        []string
}

func (s childSpawnSpec) pidPath() string { return filepath.Join(s.Root, s.Name+".pid") }
func (s childSpawnSpec) logPath() string { return filepath.Join(s.Root, s.Name+".log") }

// spawnedServerChild is a server child process observed while it boots: its
// pidfile, its own log, and whether it has exited yet.
type spawnedServerChild struct {
	t       *testing.T
	name    string
	cmd     *exec.Cmd
	pidPath string
	logPath string
	logFile *os.File

	waited  chan struct{}
	mu      sync.Mutex
	waitErr error
	logOnce sync.Once
}

// spawnServerChild starts the child with its combined output going to its OWN
// log file, not to a CombinedOutput buffer: a log FILE can be tailed while the
// child is still running, which is what makes a spinning child diagnosable at
// the moment the budget expires instead of only after it dies.
func spawnServerChild(t *testing.T, spec childSpawnSpec) *spawnedServerChild {
	t.Helper()
	logFile, err := os.Create(spec.logPath())
	if err != nil {
		t.Fatalf("attempt %d (%s): create child log %s: %v", spec.Attempt, spec.Name, spec.logPath(), err)
	}
	cmd := exec.Command(os.Args[0], "-test.run="+spec.RunPattern, "-test.v",
		fmt.Sprintf("-test.timeout=%s", spec.Timeout))
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("attempt %d (%s): start child: %v", spec.Attempt, spec.Name, err)
	}
	c := &spawnedServerChild{
		t:       t,
		name:    fmt.Sprintf("attempt %d/%d (%s)", spec.Attempt, childBootAttempts, spec.Name),
		cmd:     cmd,
		pidPath: spec.pidPath(),
		logPath: spec.logPath(),
		logFile: logFile,
		waited:  make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.waitErr = err
		c.mu.Unlock()
		close(c.waited)
	}()
	t.Cleanup(func() { c.reap() })
	return c
}

// exited reports whether the process has already returned, and with what error.
func (c *spawnedServerChild) exited() (bool, error) {
	select {
	case <-c.waited:
		c.mu.Lock()
		defer c.mu.Unlock()
		return true, c.waitErr
	default:
		return false, nil
	}
}

// exitDetail is the "it is already dead" half of a diagnosis: the exact exit
// status plus the child's own log tail, so a child that dies mid-boot is
// reported IMMEDIATELY instead of being waited on for the rest of the budget.
func (c *spawnedServerChild) exitDetail(err error) string {
	status := "its exit status is unavailable"
	if c.cmd.ProcessState != nil {
		if code := c.cmd.ProcessState.ExitCode(); code >= 0 {
			status = fmt.Sprintf("exit code %d", code)
		} else {
			// Go reports -1 when a signal ended the process; naming the signal
			// is the actionable fact, "exit code -1" is not.
			status = fmt.Sprintf("killed by a signal (%v)", c.cmd.ProcessState)
		}
	}
	why := ""
	if err != nil {
		why = fmt.Sprintf(" [%v]", err)
	}
	return fmt.Sprintf("%s (pid %d) was gone before finishing its boot%s with %s; "+
		"waited on pidfile %s.\n%s",
		c.name, c.cmd.Process.Pid, why, status, c.pidPath, childLogDiagnosis(c.logPath))
}

// budgetDetail is the "still running, budget gone" half of a diagnosis: the
// process and the pidfile it was waited on, whether the pidfile exists at all,
// the wait it expired in, and the child's own log tail.
func (c *spawnedServerChild) budgetDetail(budget time.Duration, waited string, lastErr error) string {
	return fmt.Sprintf("%s (pid %d) did not %s within %s; pidfile %s %s (last read: %v).\n%s",
		c.name, c.cmd.Process.Pid, waited, budget, c.pidPath, statNote(c.pidPath), lastErr,
		childLogDiagnosis(c.logPath))
}

func statNote(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "(absent)"
	}
	return "(present)"
}

// childExitProbe is how a caller observes the child it is waiting on: reports
// whether it has already exited and, if so, the detail to print. The
// process-child harness can see the exit status; an int-only caller can only see
// liveness. Both get the same fail-fast.
type childExitProbe func() (exited bool, detail string)

// pidfileHandshake waits for the child's pidfile to record a port and returns it
// once the pidfile provably describes childPID (INT-CI-022).
//
// INT-CI-023: two guarantees on every exit path. (1) A child that has already
// exited is reported IMMEDIATELY with its exit status and its own log tail,
// instead of being waited on for the rest of the budget. (2) An expired budget
// names the child, the elapsed wait, the pidfile it was waiting on and the tail of
// the child's own log — never a bare "never wrote its pidfile", which said
// nothing about whether the boot stalled before net.Listen, after the bind, or in
// the handshake itself.
func pidfileHandshake(name, pidPath, logPath string, childPID int, budget time.Duration, probe childExitProbe) (int, error) {
	start := time.Now()
	var lastErr error
	for {
		rec, err := pidfile.Read(pidPath)
		switch {
		case err == nil && rec.Port > 0:
			if rec.PID != childPID {
				return 0, fmt.Errorf("%w: pidfile %s records pid %d on port %d, but the child this harness "+
					"spawned is pid %d, so port %d belongs to a FOREIGN listener and every assertion below "+
					"would be about the wrong server", errForeignChildPort, pidPath, rec.PID, rec.Port,
					childPID, rec.Port)
			}
			return rec.Port, nil
		case err == nil:
			lastErr = fmt.Errorf("pidfile exists but records no port yet")
		default:
			lastErr = err
		}
		if exited, detail := probe(); exited {
			return 0, errors.New(detail)
		}
		if elapsed := time.Since(start); elapsed >= budget {
			return 0, fmt.Errorf("%s did not publish the port it bound within its %s attempt budget "+
				"(waited %s); pidfile %s %s (last read: %v).\n%s",
				name, budget, elapsed.Round(time.Millisecond), pidPath, statNote(pidPath), lastErr,
				childLogDiagnosis(logPath))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForBoundPort returns the port the child ACTUALLY bound, read from the
// pidfile the child writes only after its own listen succeeded.
func (c *spawnedServerChild) waitForBoundPort(plan childBootPlan) (int, error) {
	return pidfileHandshake(c.name, c.pidPath, c.logPath, c.cmd.Process.Pid, plan.Attempt,
		func() (bool, string) {
			if done, waitErr := c.exited(); done {
				return true, c.exitDetail(waitErr)
			}
			return false, ""
		})
}

// waitForHealth polls /health on the port the child published and returns once it
// answers 200. Same guarantees as waitForBoundPort: fail fast on exit, log tail
// on expiry.
func (c *spawnedServerChild) waitForHealth(plan childBootPlan, baseURL string) error {
	client := &http.Client{Timeout: plan.Request}
	start := time.Now()
	var lastErr error
	for {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("GET /health answered %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if done, waitErr := c.exited(); done {
			return fmt.Errorf("%s", c.exitDetail(waitErr))
		}
		if elapsed := time.Since(start); elapsed >= plan.Attempt {
			return fmt.Errorf("%s bound a port but never answered /health on %s within its %s "+
				"attempt budget (waited %s).\n%s", c.name, baseURL, plan.Attempt,
				elapsed.Round(time.Millisecond), c.budgetDetail(plan.Attempt, "answer /health", lastErr))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// awaitExit waits up to d for the process to return. It reports nil only when the
// process has actually exited (the caller then reads its status via waitErrValue).
func (c *spawnedServerChild) awaitExit(d time.Duration) error {
	select {
	case <-c.waited:
		return nil
	case <-time.After(d):
		return fmt.Errorf("still running after %s", d)
	}
}

// waitErrValue is cmd.Wait's result, safe to call once the process has exited.
func (c *spawnedServerChild) waitErrValue() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waitErr
}

// kill stops the child and reaps it. It is idempotent and never fails the test: a
// failed attempt is already being reported by the caller.
func (c *spawnedServerChild) kill() {
	if done, _ := c.exited(); !done {
		_ = c.cmd.Process.Kill()
	}
	<-c.waited
	c.closeLog()
}

// reap is the cleanup-path kill: stop the child if the test never did.
func (c *spawnedServerChild) reap() {
	if done, _ := c.exited(); !done {
		_ = c.cmd.Process.Kill()
		<-c.waited
	}
	c.closeLog()
}

func (c *spawnedServerChild) closeLog() {
	c.logOnce.Do(func() { _ = c.logFile.Close() })
}

// bootChildWithRetry spawns the child up to plan.Attempts times and returns the
// first one that published its port AND answered /health. Each attempt is a fresh
// process with a FRESH pidfile and log (spec(attempt) names them), and the failed
// attempt is killed before the next one starts: a retry may never inherit a port,
// a pidfile or a live process from the attempt that failed.
//
// A foreign-pidfile failure is NOT retryable by construction: it means the
// pidfile describes a process this test did not spawn, and retrying it would be
// exactly the "assert against a stranger" mistake the handshake exists to
// prevent.
func bootChildWithRetry(t *testing.T, plan childBootPlan, spec func(attempt int) childSpawnSpec) (*spawnedServerChild, string) {
	t.Helper()
	var failures []string
	for attempt := 1; attempt <= plan.Attempts; attempt++ {
		child := spawnServerChild(t, spec(attempt))
		port, err := child.waitForBoundPort(plan)
		if err != nil {
			child.kill()
			if errors.Is(err, errForeignChildPort) {
				t.Fatalf("%v", err)
			}
			failures = append(failures, err.Error())
			continue
		}
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		if err := child.waitForHealth(plan, base); err != nil {
			child.kill()
			failures = append(failures, err.Error())
			continue
		}
		t.Logf("%s (pid %d) published port %d and answered /health; remaining attempt budget unused "+
			"(attempt budget %s, %d allowed)", child.name, child.cmd.Process.Pid, port, plan.Attempt, plan.Attempts)
		return child, base
	}
	t.Fatalf("%s: the server child did not bootstrap in %d attempts.\n\n%s",
		childBootBudgetMarker, plan.Attempts, strings.Join(failures, "\n\n"))
	return nil, ""
}
