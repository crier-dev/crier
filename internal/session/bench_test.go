package session

// bench_test.go — the SQLite-backed session API performance baseline
// (DF-CRIER-308).
//
// Why a benchmark and not a test: a latency assertion in the default suite
// would make `go test ./...` depend on the host's load average, and ticks on
// this repo share the host with the rest of the fleet (see AGENTS.md "Load
// safety"). Benchmarks never run under `go test`, so this file costs the
// default suite nothing; it is driven explicitly:
//
//	go test -bench=BenchmarkSession -benchtime=10s ./internal/session/
//	go test -bench=BenchmarkSessionReadP99 -benchtime=10s ./internal/session/
//	scripts/benchmark-sessions.sh            # the same runs, recorded
//
// What is measured, and against what:
//
//   - the REAL route set (registerSessionRoutes), the REAL handler
//     (NewHTTPHandler) and the REAL sqlite adapter (NewSQLiteStore) — the same
//     wiring the shipped `CR_SESSION_BACKEND=sqlite` server serves, over an
//     httptest server, so the numbers include HTTP framing and routing;
//   - a pre-seeded dataset of benchSeedSessions sessions x benchSeedMessages
//     messages, so every read has real rows to assemble (an empty store
//     measures the empty-store path and proves nothing).
//
// The budget: every benchmark reports p50/p95/p99 per endpoint and prints a
// PASS / OVER verdict for each against the DF-CRIER-308 target of 100ms p99
// (CR_SESSION_P99_BUDGET_MS overrides the ceiling for another host). Setting
// CR_SESSION_ASSERT_P99=1 turns the verdict into a hard failure, so the same
// file is both a recorder and a gate.
//
// The assertion is opt-in because this repo's ticks share the host with the
// rest of the fleet (AGENTS.md "Load safety"): a wall-clock p99 measured under
// another tenant's load says more about the host than about the endpoint, and
// the MEASURED baseline in docs/performance.md records exactly that — the
// endpoints' own service times, the host's per-commit cost, and where the
// 100ms-at-100-concurrent target is and is not reachable. A benchmark that
// cannot run to completion on a busy host would be a broken tool, not a
// baseline, so the default run always records and never aborts.
//
// The write path is measured SEQUENTIALLY on purpose: NewSQLiteStore pins
// `SetMaxOpenConns(1)` (SQLite admits a single writer, and one pooled
// connection keeps the per-connection pragmas in force), so concurrent writers
// queue on one connection by design and a concurrent-create p99 measures the
// queue rather than the write. Concurrent load (the p99 gate) therefore
// exercises the READ endpoints, which is what "under moderate load" means for
// a latency budget.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/registry"
)

const (
	// benchClients is the "moderate load" of the DF-CRIER-308 brief: 100
	// concurrent requests in flight.
	benchClients = 100
	// benchSeedSessions / benchSeedMessages size the fixture every read
	// assembles. The list endpoint is O(N) in sessions (one Load per session,
	// see HandleListSessions), so this number is also the list endpoint's
	// scaling input — measured and recorded in docs/performance.md.
	benchSeedSessions = 20
	benchSeedMessages = 20

	benchAgentA = "atlas"
	benchAgentB = "nimbus"

	// benchDirPrefix names the store directories this file owns in $TMPDIR.
	benchDirPrefix = "crier-session-bench-"
)

// benchBaseTime is a fixed clock so no benchmark depends on wall time.
var benchBaseTime = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)

// benchCreateBody is the POST /sessions body the create benchmarks replay. It
// states NO id, so the handler mints one per request and every iteration is a
// real create (a replayed fixed id would answer 409).
var benchCreateBody = []byte(`{"title":"bench-room","kind":"channel",` +
	`"created_by":{"agent":"` + benchAgentA + `"},` +
	`"members":[` +
	`{"member_type":"agent","member_id":"` + benchAgentA + `","role":"owner"},` +
	`{"member_type":"agent","member_id":"` + benchAgentB + `","role":"member"}]}`)

// benchReq is one pre-built request the drivers replay. paths is a rotation so
// a read can spread across the seeded sessions.
type benchReq struct {
	name   string
	method string
	paths  []string
	body   []byte
}

// benchEnv is the in-process session API under test.
type benchEnv struct {
	srv    *httptest.Server
	store  Repository
	client *http.Client
}

// benchEnvOnce returns the process-wide env for one benchmark, creating it on
// the first call and reusing it for every later CALIBRATION ROUND.
//
// The scope is deliberate and load-bearing. go test invokes the benchmark
// function once per round (b.N = 1, then the target), and a b.Cleanup-registered
// teardown runs at the END OF EACH ROUND — so a fixture that registered its own
// cleanup was deleted and re-seeded on every round. Re-seeding is not cheap
// here: the fixture is ~440 durable record writes, which is ~11-25s with the
// shipped rollback journal (the very cost this baseline measures). Measured:
// one `-benchtime=100x` run of a single cell opened the env TWICE and spent 72s
// doing it. The env therefore lives for the life of the test binary — whose
// whole job is the measurement — and each benchmark gets its OWN env (keyed by
// name) so a write benchmark cannot grow the dataset a read benchmark measures.
var (
	benchEnvMu    sync.Mutex
	benchEnvCache = map[string]*benchEnv{}
)

func benchEnvOnce(b *testing.B, name string) *benchEnv {
	benchEnvMu.Lock()
	defer benchEnvMu.Unlock()
	if e, ok := benchEnvCache[name]; ok {
		return e
	}
	e := newBenchEnv(b, name)
	benchEnvCache[name] = e
	return e
}

// benchStoreDir returns the store directory for this run and reaps any left
// behind by an EARLIER, now-dead benchmark process.
//
// The env outlives every calibration round by design (benchEnvOnce), so the
// PROCESS owns the directory, not the harness — and the process cannot run its
// own exit hook from inside a benchmark. The pid is therefore part of the name:
// a directory whose pid is still alive belongs to a concurrent run and is left
// alone, and one whose pid is gone is removed before a new directory is made,
// so running the benchmark repeatedly does not accumulate stores in $TMPDIR.
func benchStoreDir(b *testing.B, name string) string {
	b.Helper()
	self := os.Getpid()
	if stale, err := filepath.Glob(filepath.Join(os.TempDir(), benchDirPrefix+"*-"+name+"-*")); err == nil {
		for _, dir := range stale {
			// crier-session-bench-<pid>-<name>-<random>
			fields := strings.Split(filepath.Base(dir), "-")
			if len(fields) < 5 {
				continue
			}
			pid, err := strconv.Atoi(fields[3])
			if err != nil || pid == self {
				continue
			}
			if proc, err := os.FindProcess(pid); err == nil && proc.Signal(syscall.Signal(0)) == nil {
				continue // still running: not ours to remove
			}
			_ = os.RemoveAll(dir)
		}
	}
	dir, err := os.MkdirTemp("", fmt.Sprintf("%s%d-%s-", benchDirPrefix, self, name))
	if err != nil {
		b.Fatalf("temp dir: %v", err)
	}
	return dir
}

// newBenchEnv opens the real sqlite session store, builds the real handler over
// the real route set and seeds the fixture.
//
// The database lives in a directory this function owns (NOT b.TempDir, whose
// cleanup the harness runs after every round — see benchEnvOnce); it is a few
// hundred KB and benchStoreDir reaps it on the next run.
func newBenchEnv(b *testing.B, name string) *benchEnv {
	b.Helper()

	dir := benchStoreDir(b, name)
	store, err := NewSQLiteStore(filepath.Join(dir, "sessions.sqlite"))
	if err != nil {
		b.Fatalf("open sqlite session store: %v", err)
	}
	reg := registry.NewMemoryStore()
	for _, id := range []string{benchAgentA, benchAgentB} {
		if err := reg.Register(&registry.Agent{ID: id}); err != nil {
			b.Fatalf("register %s: %v", id, err)
		}
	}
	h := NewHTTPHandler(store, HTTPOptions{Deliverer: registry.NewSessionDeliverer(registry.NewHandler(reg)), Agents: reg})
	r := mux.NewRouter()
	registerSessionRoutes(r, h)
	srv := httptest.NewServer(r)

	// A pooled transport sized for the concurrent gate: without it every
	// request opens a connection and the measurement is the dialer.
	tr := &http.Transport{
		MaxIdleConns:        4 * benchClients,
		MaxIdleConnsPerHost: 4 * benchClients,
		IdleConnTimeout:     60 * time.Second,
	}
	e := &benchEnv{srv: srv, store: store, client: &http.Client{Transport: tr}}
	// No b.Cleanup teardown: it would run at the end of THIS round and close
	// the store the next round measures against (see benchEnvOnce). The server,
	// the transport and the database are released when the test binary exits.
	e.seed(b)
	return e
}

// seed writes benchSeedSessions sessions, each with one agent member and
// benchSeedMessages thread-root messages, through the Repository the handler
// serves from — the same rows a live deployment would carry.
func (e *benchEnv) seed(b *testing.B) {
	b.Helper()
	ctx := context.Background()
	for i := 0; i < benchSeedSessions; i++ {
		id := fmt.Sprintf("bench-%03d", i)
		at := benchBaseTime.Add(time.Duration(i) * time.Millisecond)
		sess := &Session{
			ID:        id,
			Kind:      KindChannel,
			Title:     "bench room",
			CreatedAt: at,
			CreatedBy: AuthorRef{Agent: benchAgentA},
			State:     SessionOpen,
		}
		if err := e.store.CreateSession(ctx, sess, 1); err != nil {
			b.Fatalf("seed session %s: %v", id, err)
		}
		member := &Member{
			SessionID:  id,
			MemberType: MemberAgent,
			MemberID:   benchAgentA,
			Role:       RoleOwner,
			AddedAt:    at,
			AddedBy:    benchAgentA,
		}
		if err := e.store.AddMember(ctx, member, 2, &ContextShare{Mode: ShareSummary}); err != nil {
			b.Fatalf("seed member %s: %v", id, err)
		}
		for j := 0; j < benchSeedMessages; j++ {
			mid := fmt.Sprintf("%s-m%02d", id, j)
			msg := &Message{
				ID:        mid,
				ThreadID:  mid,
				SessionID: id,
				Kind:      MessagePlain,
				Author:    AuthorRef{Agent: benchAgentA},
				Payload:   json.RawMessage(`{"text":"benchmark payload"}`),
				Audience: Audience{
					Rule:    AudienceSession,
					Targets: []AudienceTarget{{Kind: TargetAgent, ID: benchAgentB}},
				},
				Seq:       int64(3 + j),
				CreatedAt: at.Add(time.Duration(j) * time.Second),
			}
			if err := e.store.PostMessage(ctx, msg); err != nil {
				b.Fatalf("seed message %s: %v", mid, err)
			}
		}
	}
}

// seedPaths returns the transcript read path of every seeded session.
func (e *benchEnv) seedPaths() []string {
	out := make([]string, 0, benchSeedSessions)
	for i := 0; i < benchSeedSessions; i++ {
		out = append(out, fmt.Sprintf("/sessions/bench-%03d/messages", i))
	}
	return out
}

// do issues one request against the in-process server and drains the body.
func (e *benchEnv) do(method, path string, body []byte) (int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// benchmark requests, one per endpoint measured.
func (e *benchEnv) createReq() benchReq {
	return benchReq{name: "create", method: http.MethodPost, paths: []string{"/sessions"}, body: benchCreateBody}
}

func (e *benchEnv) retrieveReq() benchReq {
	return benchReq{name: "retrieve", method: http.MethodGet, paths: e.seedPaths()}
}

func (e *benchEnv) listReq() benchReq {
	return benchReq{name: "list", method: http.MethodGet, paths: []string{"/sessions"}}
}

// benchDuration is one timed request, tagged with the endpoint it hit.
type benchDuration struct {
	name string
	d    time.Duration
}

// benchRunSequential replays reqs for b.N iterations on one goroutine and
// returns the durations per endpoint.
func benchRunSequential(b *testing.B, env *benchEnv, reqs []benchReq) map[string][]time.Duration {
	b.Helper()
	for _, r := range reqs {
		if status, err := env.do(r.method, r.paths[0], r.body); err != nil || status/100 != 2 {
			b.Fatalf("warm-up %s: status=%d err=%v", r.name, status, err)
		}
	}
	out := map[string][]time.Duration{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := reqs[i%len(reqs)]
		path := r.paths[i%len(r.paths)]
		start := time.Now()
		status, err := env.do(r.method, path, r.body)
		d := time.Since(start)
		if err != nil || status/100 != 2 {
			b.Fatalf("%s %s: status=%d err=%v", r.method, path, status, err)
		}
		out[r.name] = append(out[r.name], d)
	}
	b.StopTimer()
	return out
}

// benchRunConcurrent replays reqs for b.N iterations spread over clients
// goroutines (each goroutine holds one request in flight, so the load is
// exactly `clients` wide) and returns the durations per endpoint.
func benchRunConcurrent(b *testing.B, env *benchEnv, reqs []benchReq, clients int) map[string][]time.Duration {
	b.Helper()
	if clients < 1 {
		clients = 1
	}
	for _, r := range reqs {
		if status, err := env.do(r.method, r.paths[0], r.body); err != nil || status/100 != 2 {
			b.Fatalf("warm-up %s: status=%d err=%v", r.name, status, err)
		}
	}

	per := make([][]benchDuration, clients)
	var next, failures int64
	var wg sync.WaitGroup
	b.ResetTimer()
	for w := 0; w < clients; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var local []benchDuration
			for {
				i := int(atomic.AddInt64(&next, 1)) - 1
				if i >= b.N {
					break
				}
				r := reqs[i%len(reqs)]
				path := r.paths[i%len(r.paths)]
				start := time.Now()
				status, err := env.do(r.method, path, r.body)
				d := time.Since(start)
				if err != nil || status/100 != 2 {
					atomic.AddInt64(&failures, 1)
					continue
				}
				local = append(local, benchDuration{name: r.name, d: d})
			}
			per[w] = local
		}(w)
	}
	wg.Wait()
	b.StopTimer()
	if n := atomic.LoadInt64(&failures); n > 0 {
		b.Fatalf("concurrent load: %d of %d requests failed", n, b.N)
	}

	out := map[string][]time.Duration{}
	for _, local := range per {
		for _, s := range local {
			out[s.name] = append(out[s.name], s.d)
		}
	}
	return out
}

// benchP99Budget is the p99 ceiling every benchmark measures against: the
// DF-CRIER-308 target of 100ms, overridable for another host.
func benchP99Budget() time.Duration {
	if raw := os.Getenv("CR_SESSION_P99_BUDGET_MS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 100 * time.Millisecond
}

// benchAssertP99 reports whether an over-budget p99 should FAIL the run.
// Default OFF: a recording run on a host shared with the rest of the fleet
// must still complete. The verdict is printed either way, so silence is never
// mistaken for a pass.
func benchAssertP99() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CR_SESSION_ASSERT_P99"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// benchPercentile returns the nearest-rank percentile of a sorted slice.
func benchPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// benchMillis renders a duration as fractional milliseconds, the unit the
// BASELINE contract and every reported metric use.
func benchMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// benchReport logs the per-endpoint distribution, publishes it as benchmark
// metrics and prints a PASS / OVER verdict per endpoint against the p99 budget.
// With CR_SESSION_ASSERT_P99=1 an over-budget endpoint fails the run; by
// default the verdict is reported, never swallowed. It returns the overall
// (all endpoints pooled) p99.
//
// mode names the load the samples were taken under — "seq" for the
// one-request-at-a-time cells and "conc100" for the benchClients-concurrent
// ones — and is part of the BASELINE contract, because the same endpoint
// measured sequentially and 100-concurrently are two different numbers and a
// summary that merged them would be unreadable.
func benchReport(b *testing.B, mode string, samples map[string][]time.Duration) time.Duration {
	b.Helper()
	if len(samples) == 0 {
		b.Fatal("no samples collected")
	}
	budget := benchP99Budget()

	names := make([]string, 0, len(samples))
	for n := range samples {
		names = append(names, n)
	}
	sort.Strings(names)

	var all []time.Duration
	var over int
	for _, n := range names {
		ds := samples[n]
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		all = append(all, ds...)

		p50 := benchPercentile(ds, 0.50)
		p95 := benchPercentile(ds, 0.95)
		p99 := benchPercentile(ds, 0.99)
		verdict := "PASS"
		if p99 > budget {
			verdict = "OVER"
			over++
		}
		b.Logf("endpoint %-9s mode=%-7s n=%-7d p50=%-10s p95=%-10s p99=%-10s max=%-10s verdict=%s",
			n, mode, len(ds), p50.Round(time.Microsecond), p95.Round(time.Microsecond),
			p99.Round(time.Microsecond), ds[len(ds)-1].Round(time.Microsecond), verdict)
		// One machine-readable line per (endpoint, mode, round) so
		// scripts/benchmark-sessions.sh can record the baseline without parsing
		// prose. Shape is a contract:
		// BASELINE <endpoint> mode=<seq|conc100> n=<int> p50_ms=<f> p95_ms=<f> p99_ms=<f> max_ms=<f> verdict=<PASS|OVER> budget_ms=<int>
		b.Logf("BASELINE %s mode=%s n=%d p50_ms=%.3f p95_ms=%.3f p99_ms=%.3f max_ms=%.3f verdict=%s budget_ms=%d",
			n, mode, len(ds), benchMillis(p50), benchMillis(p95), benchMillis(p99),
			benchMillis(ds[len(ds)-1]), verdict, budget.Milliseconds())
		b.ReportMetric(benchMillis(p50), "p50_ms_"+n)
		b.ReportMetric(benchMillis(p95), "p95_ms_"+n)
		b.ReportMetric(benchMillis(p99), "p99_ms_"+n)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	overall99 := benchPercentile(all, 0.99)
	b.ReportMetric(float64(overall99.Microseconds())/1000, "p99_ms_all")
	b.Logf("every endpoint pooled: n=%d p50=%s p95=%s p99=%s", len(all),
		benchPercentile(all, 0.50).Round(time.Microsecond),
		benchPercentile(all, 0.95).Round(time.Microsecond),
		overall99.Round(time.Microsecond))
	b.Logf("p99 budget %s (DF-CRIER-308 target): %d of %d endpoints OVER — "+
		"see docs/performance.md for the recorded baseline and its causes",
		budget, over, len(names))

	if over > 0 && benchAssertP99() {
		b.Fatalf("session API p99 budget exceeded: %d of %d endpoints over %s "+
			"(CR_SESSION_ASSERT_P99 is set)", over, len(names), budget)
	}
	return overall99
}

// BenchmarkSessionCreate measures POST /sessions (the write path) sequentially.
// Each iteration creates a NEW session — the handler mints the id, so the body
// is constant and replayed.
func BenchmarkSessionCreate(b *testing.B) {
	env := benchEnvOnce(b, "create-seq")
	samples := benchRunSequential(b, env, []benchReq{env.createReq()})
	benchReport(b, "seq", samples)
}

// BenchmarkSessionRetrieve measures GET /sessions/{id}/messages — the ordered
// cross-agent transcript, the session OBJECT read (rows 11-14). No bare
// GET /sessions/{id} is registered; this is that read.
func BenchmarkSessionRetrieve(b *testing.B) {
	env := benchEnvOnce(b, "retrieve-seq")
	samples := benchRunSequential(b, env, []benchReq{env.retrieveReq()})
	benchReport(b, "seq", samples)
}

// BenchmarkSessionList measures GET /sessions over the seeded dataset.
func BenchmarkSessionList(b *testing.B) {
	env := benchEnvOnce(b, "list-seq")
	samples := benchRunSequential(b, env, []benchReq{env.listReq()})
	benchReport(b, "seq", samples)
}

// BenchmarkSessionReadP99 measures the READ endpoints under benchClients
// concurrent requests in flight — the "moderate load" of the DF-CRIER-308 brief
// and the load gate for the 100ms p99 target.
func BenchmarkSessionReadP99(b *testing.B) {
	env := benchEnvOnce(b, "read-p99")
	samples := benchRunConcurrent(b, env, []benchReq{env.retrieveReq(), env.listReq()}, benchClients)
	benchReport(b, "conc100", samples)
}

// BenchmarkSessionCreateP99 measures the WRITE endpoint under the same
// concurrency. It is expected to be the slowest cell: NewSQLiteStore pins one
// connection, so concurrent creates serialize by design. Run it bounded
// (`-benchtime=500x`) — every iteration leaves a session behind.
func BenchmarkSessionCreateP99(b *testing.B) {
	env := benchEnvOnce(b, "create-p99")
	samples := benchRunConcurrent(b, env, []benchReq{env.createReq()}, benchClients)
	benchReport(b, "conc100", samples)
}

// BenchmarkSessionStoreCommitCost measures the floor under every write
// endpoint: what ONE durable write costs the store, with the pragmas
// sqliteDialect actually arms, before any handler logic. Each iteration issues
// the two statements SQLStore.Append issues for one record (the projection
// INSERT plus the chat_seq upsert) as two implicit transactions, so the number
// is directly comparable to the per-record cost inside POST /sessions.
//
// It is measured HERE, beside the endpoints, because the baseline's headline
// finding is a property of this cost and not of the HTTP layer: with the
// shipped rollback journal a commit is ~3 orders of magnitude more expensive
// than one with WAL, and a POST /sessions performs several commits. The two
// arms are benchmarks, not a configuration: the SHIPPED arm is the store as it
// runs today, and the WAL arm costs the recommended change (docs/performance.md
// "Remediation") so the recommendation is measured rather than asserted. The
// WAL arm writes its own database file and changes nothing about the store.
func BenchmarkSessionStoreCommitCost(b *testing.B) {
	cases := []struct {
		name    string
		pragmas []string
	}{
		// The shipped shape: sqliteDialect.InitStatements() and nothing else.
		{name: "rollback", pragmas: nil},
		// The recommended change, applied the way the store would have to
		// apply it (journal_mode returns a row, so it cannot ride
		// InitStatements).
		{name: "wal", pragmas: []string{"PRAGMA journal_mode=WAL"}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			db := benchCommitDB(b, tc.pragmas)
			ds := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				if _, err := db.Exec(
					`INSERT INTO commit_cost (k, v) VALUES (?, ?)`,
					fmt.Sprintf("k-%d", i), i); err != nil {
					b.Fatalf("project insert: %v", err)
				}
				if _, err := db.Exec(
					`INSERT INTO commit_cost (k, v) VALUES (?, ?)
					 ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
					"seq", i); err != nil {
					b.Fatalf("seq upsert: %v", err)
				}
				ds = append(ds, time.Since(start))
			}
			b.StopTimer()
			benchReport(b, "seq", map[string][]time.Duration{"commit-" + tc.name: ds})
		})
	}
}

// benchCommitDB opens a store-shaped connection: the same driver, the same
// single-connection pooling and the same dialect pragmas NewSQLiteStore arms,
// plus any extra pragma an arm adds.
func benchCommitDB(b *testing.B, extra []string) *sql.DB {
	b.Helper()
	db, err := sql.Open(sqliteDriverName, filepath.Join(b.TempDir(), "commit-cost.sqlite"))
	if err != nil {
		b.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, stmt := range (sqliteDialect{}).InitStatements() {
		if _, err := db.Exec(stmt); err != nil {
			b.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, stmt := range extra {
		if _, err := db.Exec(stmt); err != nil {
			b.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE commit_cost (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`); err != nil {
		b.Fatalf("create table: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}
