package ratelimit

import (
	"testing"
	"time"
)

// ratelimit_test.go pins the two answers the ingest lanes shed with (CR-FEAT-035):
// whether an attempt is admitted, and — when it is not — how long the caller
// should wait. The second one is the whole point of the package: it is what the
// 429's `Retry-After` reports, so it has to be an honest, waitable number rather
// than a constant.

func TestWindowAdmitsUpToTheLimitThenSheds(t *testing.T) {
	w := NewWindow(time.Minute)

	for i := 0; i < 3; i++ {
		if !w.Allow("agent-1", 3, time.Minute) {
			t.Fatalf("attempt %d should be admitted under a limit of 3", i)
		}
	}
	if w.Allow("agent-1", 3, time.Minute) {
		t.Fatal("the attempt past the limit must be shed")
	}
}

func TestWindowKeysAreIndependent(t *testing.T) {
	w := NewWindow(time.Minute)

	for i := 0; i < 3; i++ {
		w.Allow("agent-1", 3, time.Minute)
	}
	if w.Allow("agent-1", 3, time.Minute) {
		t.Fatal("agent-1 should be over its budget")
	}
	if !w.Allow("agent-2", 3, time.Minute) {
		t.Fatal("a different key has its own budget")
	}
}

func TestWindowDisabledLimitAdmitsEverything(t *testing.T) {
	w := NewWindow(time.Minute)

	for i := 0; i < 1000; i++ {
		if !w.Allow("agent-1", 0, time.Minute) {
			t.Fatalf("limit 0 disables the limit; attempt %d was shed", i)
		}
		if !w.Allow("agent-1", -1, time.Minute) {
			t.Fatalf("a negative limit is also 'disabled'; attempt %d was shed", i)
		}
	}
	if got := w.RetryAfter("agent-1", 0, time.Minute); got != 0 {
		t.Fatalf("a disabled limit owes no wait, got %s", got)
	}
}

func TestWindowRetryAfterIsZeroWhileUnderBudget(t *testing.T) {
	w := NewWindow(time.Minute)

	if got := w.RetryAfter("agent-1", 3, time.Minute); got != 0 {
		t.Fatalf("nothing has been attempted yet, so nothing is owed: %s", got)
	}
	w.Allow("agent-1", 3, time.Minute)
	if got := w.RetryAfter("agent-1", 3, time.Minute); got != 0 {
		t.Fatalf("one attempt under a limit of three owes no wait: %s", got)
	}
}

// TestWindowRetryAfterIsWaitable is the acceptance property of the header: a
// caller that was shed, waits exactly what the window told it to wait, and
// retries (with nobody else competing) is admitted.
func TestWindowRetryAfterIsWaitable(t *testing.T) {
	const (
		limit  = 2
		window = 200 * time.Millisecond
	)
	w := NewWindow(time.Minute)

	for i := 0; i < limit; i++ {
		if !w.Allow("producer", limit, window) {
			t.Fatalf("attempt %d should be admitted", i)
		}
	}
	if w.Allow("producer", limit, window) {
		t.Fatal("the attempt past the limit must be shed")
	}

	wait := w.RetryAfter("producer", limit, window)
	if wait <= 0 {
		t.Fatalf("a shed caller must be owed a positive wait, got %s", wait)
	}
	if wait > window {
		t.Fatalf("the wait %s must not exceed the window %s it is waiting out", wait, window)
	}
	// RetryAfter itself must not consume budget: two calls answer the same.
	if again := w.RetryAfter("producer", limit, window); again > wait {
		t.Fatalf("asking what is owed must not lengthen the wait: %s then %s", wait, again)
	}

	// Wait it out (plus a margin: a timer that fires late can only age MORE
	// attempts out, which is the safe direction), then retry.
	time.Sleep(wait + 50*time.Millisecond)
	if !w.Allow("producer", limit, window) {
		t.Fatalf("a caller that waited the reported %s was shed again", wait)
	}
}

// TestWindowRetryAfterShrinksAsTheWindowSlides pins the countdown: the wait
// reported later is shorter than the wait reported at the moment of the shed.
func TestWindowRetryAfterShrinksAsTheWindowSlides(t *testing.T) {
	const (
		limit  = 1
		window = 500 * time.Millisecond
	)
	w := NewWindow(time.Minute)

	w.Allow("producer", limit, window)
	if w.Allow("producer", limit, window) {
		t.Fatal("the second attempt must be shed")
	}
	first := w.RetryAfter("producer", limit, window)

	time.Sleep(150 * time.Millisecond)
	second := w.RetryAfter("producer", limit, window)

	if first <= 0 || second <= 0 {
		t.Fatalf("both reads must owe a wait, got %s then %s", first, second)
	}
	if second >= first {
		t.Fatalf("the wait must shrink as the window slides: %s then %s", first, second)
	}
}

// TestWindowCleanupKeepsActiveBudgetsIntact pins that the background sweep is
// conservative: an entry whose hits are still inside a minute-long window must
// survive a sweep interval of milliseconds, so a live budget is never reset by
// housekeeping.
func TestWindowCleanupKeepsActiveBudgetsIntact(t *testing.T) {
	w := NewWindow(20 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if !w.Allow("agent-1", 3, time.Minute) {
			t.Fatalf("attempt %d should be admitted", i)
		}
	}
	time.Sleep(200 * time.Millisecond)

	if w.Allow("agent-1", 3, time.Minute) {
		t.Fatal("cleanup must not reset a budget whose window is still open")
	}
}

func TestHeaderSecondsRoundsUpAndNeverReadsAsRetryNow(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want int
	}{
		{0, 1},                      // a shed happened: "retry in 0s" means retry now
		{-time.Second, 1},           // never a negative header
		{300 * time.Millisecond, 1}, // rounds UP to 1
		{time.Second, 1},
		{time.Second + time.Nanosecond, 2},
		{59 * time.Second, 59},
		{60 * time.Second, 60},
	} {
		if got := HeaderSeconds(tc.in); got != tc.want {
			t.Errorf("HeaderSeconds(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWindowIsSafeForConcurrentCallers drives Allow and RetryAfter from several
// goroutines at once, on the same key and across keys. The property under test is
// the one the shed path depends on: the window is shared mutable state (a
// per-key slice another caller appends to), so both reads and the retry-after
// computation must happen under its lock. Run with -race it is a real check; run
// without, it at least proves the calls do not deadlock or panic.
func TestWindowIsSafeForConcurrentCallers(t *testing.T) {
	w := NewWindow(50 * time.Millisecond)

	const (
		workers    = 8
		iterations = 200
	)
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		key := "key-" + string(rune('a'+i%3))
		go func(key string) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < iterations; j++ {
				w.Allow(key, 5, 50*time.Millisecond)
				if got := w.RetryAfter(key, 5, 50*time.Millisecond); got < 0 {
					t.Errorf("RetryAfter returned a negative wait: %s", got)
					return
				}
			}
		}(key)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
}
