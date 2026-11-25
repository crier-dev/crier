package main

// DF-CRIER-515 — the lost-key recovery, end to end on the REAL server.
//
// The row's live hit (dogfood tick crier-dogfood-2026-11-24-19-47-57): a
// durable backend carried an id whose registered key file no longer existed;
// the 409 public_key_mismatch message said "delete the agent first (DELETE
// /agents/{id})", but that DELETE requires the signature of the REGISTERED
// key — exactly the key that was lost — so the remedy answered 401 and the id
// was a permanent tombstone.
//
// The fix under test here: DELETE /agents/{id} also opens with the OPERATOR
// admin token (CR_PERMISSIONS_ADMIN_TOKEN) as its Bearer, and the fix is
// armed INDEPENDENTLY of the delivery ACL — this boot deliberately leaves
// CR_PERMISSIONS_ENABLED unset, because that is the posture the row was hit
// on. The unit-level contract (handler + router, sig enforcement on, the
// unarmed and wrong-Bearer refusals) lives in
// internal/registry/lost_key_recovery_test.go; THIS gate proves the wiring —
// shared auth middleware, route table and handler — answers the same flow a
// client following only the 409 message and the README would issue.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	df515MessageToken = "df515-message-secret"
	df515AdminToken   = "df515-admin-secret"
)

// Two distinct 64-hex ed25519 public keys (the wire's spelled form): the key
// the tombstoned row was registered with, and the fresh one crier keygen
// mints afterwards.
var (
	df515Key1 = strings.Repeat("ab", 32)
	df515Key2 = strings.Repeat("cd", 32)
)

func TestLostKeyRecoveryEndToEnd(t *testing.T) {
	parallelBoot(t)
	// The ACL is deliberately NOT enabled (no CR_PERMISSIONS_ENABLED): the
	// operator credential must arm the registry delete on its own.
	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":              df515MessageToken,
		"CR_PERMISSIONS_ADMIN_TOKEN": df515AdminToken,
	})
	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Fresh registration (the pre-tombstone world): the id binds its key.
	status, body := df297Request(t, client, http.MethodPost, baseURL+"/agents",
		df515MessageToken, "", `{"id":"tombstone-e2e","public_key":"`+df515Key1+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("fresh registration = %d, want 201 (body: %s)", status, body)
	}

	// 2. A later registration with a DIFFERENT key (the key file was lost and
	//    crier keygen minted a new one) is refused with the structured 409.
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/agents",
		df515MessageToken, "", `{"id":"tombstone-e2e","public_key":"`+df515Key2+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("fresh-key re-registration = %d, want 409 (body: %s)", status, body)
	}
	if got, _ := df297JSON(t, body)["error"].(string); got != "public_key_mismatch" {
		t.Fatalf("conflict error = %q, want public_key_mismatch", got)
	}
	if msg, _ := df297JSON(t, body)["message"].(string); !strings.Contains(msg, "CR_PERMISSIONS_ADMIN_TOKEN") {
		t.Fatalf("409 message must name the operator credential of the recovery: %s", msg)
	}

	// 3. The message token is NOT the operator credential: a Bearer of it —
	//    with no signature, which is all a lost-key holder can send — is
	//    refused and deletes nothing.
	status, body = df297Request(t, client, http.MethodDelete, baseURL+"/agents/tombstone-e2e",
		df515MessageToken, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("DELETE with the message token and no signature = %d, want 401 (body: %s)", status, body)
	}

	// 4. The documented recovery: the same DELETE with the operator admin
	//    token as Bearer deletes the tombstoned identity.
	status, body = df297Request(t, client, http.MethodDelete, baseURL+"/agents/tombstone-e2e",
		df515AdminToken, "", "")
	if status != http.StatusNoContent {
		t.Fatalf("operator delete = %d, want 204 (body: %s)", status, body)
	}

	// 5. The documented happy path now completes: the SAME fresh key
	//    registers under the freed id.
	status, body = df297Request(t, client, http.MethodPost, baseURL+"/agents",
		df515MessageToken, "", `{"id":"tombstone-e2e","public_key":"`+df515Key2+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("re-registration after the recovery = %d, want 201 (body: %s)", status, body)
	}
}
