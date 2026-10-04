package daggerctl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
)

func sampleRecord(id string, state daggerctl.RunState, at time.Time) *daggerctl.RunRecord {
	return &daggerctl.RunRecord{
		RunID:           id,
		State:           state,
		RequestingAgent: "agent-1",
		Kind:            daggerctl.KindPrompt,
		Prompt:          "p",
		Evidence:        []string{"e1"},
		CreatedAt:       at,
		UpdatedAt:       at,
	}
}

func TestMemoryStoreReturnsCopies(t *testing.T) {
	store := daggerctl.NewMemoryStore()
	ctx := context.Background()
	now := time.Now()

	rec := sampleRecord("run-1", daggerctl.StateRunning, now)
	if err := store.Append(ctx, rec); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Mutating the caller's record after the append must not change the store.
	rec.State = daggerctl.StateSucceeded
	got, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != daggerctl.StateRunning {
		t.Errorf("stored state = %q, want running (the store must keep its own copy)", got.State)
	}
	// Mutating a returned record must not change the store either.
	got.State = daggerctl.StateFailed
	again, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.State != daggerctl.StateRunning {
		t.Errorf("stored state = %q after mutating a returned copy", again.State)
	}
}

func TestStoreRefusesARecordWithNoRunID(t *testing.T) {
	ctx := context.Background()
	for name, store := range map[string]daggerctl.Store{
		"memory": daggerctl.NewMemoryStore(),
		"jsonl":  newJSONLStore(t),
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Append(ctx, &daggerctl.RunRecord{}); !errors.Is(err, daggerctl.ErrInvalidInput) {
				t.Fatalf("Append(no id) = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func newJSONLStore(t *testing.T) *daggerctl.JSONLStore {
	t.Helper()
	store, err := daggerctl.NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	return store
}

func TestJSONLStoreKeepsTheLatestVersionAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	first, err := daggerctl.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	if err := first.Append(ctx, sampleRecord("run-1", daggerctl.StateRunning, now)); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	updated := sampleRecord("run-1", daggerctl.StateSucceeded, now)
	updated.Notified = true
	updated.UpdatedAt = now.Add(time.Second)
	if err := first.Append(ctx, updated); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	// A SECOND store over the same directory must see the log, not memory.
	second, err := daggerctl.NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := second.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.State != daggerctl.StateSucceeded || !got.Notified {
		t.Fatalf("record after reopen = %+v, want the LATEST version", got)
	}
	if got.RequestingAgent != "agent-1" {
		t.Errorf("reopened record lost its requesting agent: %+v", got)
	}

	if _, err := second.Get(ctx, "nope"); !errors.Is(err, daggerctl.ErrRunNotFound) {
		t.Fatalf("Get(unknown) = %v, want ErrRunNotFound", err)
	}

	runs, err := second.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-1" {
		t.Fatalf("List = %+v, want one run with keep-LAST applied", runs)
	}
}

func TestJSONLStoreListsRunsInCreationOrder(t *testing.T) {
	store := newJSONLStore(t)
	ctx := context.Background()
	base := time.Now()
	for i, id := range []string{"run-b", "run-a"} {
		if err := store.Append(ctx, sampleRecord(id, daggerctl.StateRunning, base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 2 || runs[0].RunID != "run-b" || runs[1].RunID != "run-a" {
		t.Fatalf("List order = %+v, want creation order run-b then run-a", runs)
	}
}

func TestJSONLStoreRefusesAnEmptyDirectory(t *testing.T) {
	if _, err := daggerctl.NewJSONLStore("  "); !errors.Is(err, daggerctl.ErrInvalidInput) {
		t.Fatalf("NewJSONLStore(\"\") = %v, want ErrInvalidInput", err)
	}
}
