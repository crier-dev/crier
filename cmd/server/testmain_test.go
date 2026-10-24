package main

import (
	"os"
	"testing"
)

// TestMain gives this package's tests their OWN delivery budget instead of the
// shipped 30s session / RUNTIME default.
//
// Why: `make test` runs the whole suite in ONE binary per package with
// `-timeout 60s` (Makefile:110), so every test in this file's package shares a
// 60s wall-clock budget. The shipped defaults are 30000ms (registry
// DefaultSessionDeliveryTimeoutMs) and 20s runtime; a test whose subject is "a
// failed endpoint is refused, not delivered" therefore spent up to the whole
// default budget WAITING for a retry schedule before it could assert anything —
// measured 30.2s for TestSessionFanoutWebhookFailureIsRefused alone, over half
// the budget of the package it lives in, and none of it measured the product:
// the outcome ("refused", "no reply row") is the same at 1.2s as at 30s.
//
// These are the same configuration knobs a deployment sets, applied once to the
// whole test process, which is exactly the isolation these tests were missing:
// the budget is now a property of the TEST RUN, not of each test's patience.
// A test that MEASURES the budget still sets its own value with t.Setenv (e.g.
// session_budget_test.go: 400ms, session_dispatch_live_test.go: 2000/5000ms,
// session_retry_test.go: a 6s answer against a 10s budget), because
// StartTestServerWithEnv applies the caller's overrides after its own scrub and
// config.resolveOrderedEnv prefers the process value each boot.
//
// The values keep the product invariant config enforces (runtime strictly less
// than the session budget) with room for one in-budget retry (backoff 1s) plus
// the loopback round trips a refused delivery performs.
const (
	testRuntimeTimeoutMs         = "800"
	testSessionDeliveryTimeoutMs = "1500"
)

func TestMain(m *testing.M) {
	// os.Setenv, not t.Setenv: this is the process default every boot reads,
	// and a test that asserts a different budget overrides it locally.
	for k, v := range map[string]string{
		"RUNTIME_TIMEOUT_MS":             testRuntimeTimeoutMs,
		"CR_SESSION_DELIVERY_TIMEOUT_MS": testSessionDeliveryTimeoutMs,
	} {
		if err := os.Setenv(k, v); err != nil {
			panic("set test-scoped delivery budget " + k + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}
