# CRIER-PROTOCOLS — First-class protocol adapter layer (A2A included)

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 (multi-protocol/harness support) · INT-007

## 1. Purpose

Every HTTP protocol surface in crier is registered ad-hoc in `cmd/server/main.go` against the
gorilla/mux router, each with its own gate boolean and its own config fields. There is no
`Protocol` interface; adding a protocol means editing `runWithEnv` in three places. This spec
defines a first-class protocol adapter layer — one `Protocol` contract, one closed name set,
one registration loop — and moves A2A onto it as a first-class protocol.

**Premise verification (measured at HEAD `7e4c3f4`, 2026-11-08).** The bakeoff cited
`internal/daggerctl/daggerctl.go:8` for a "single executor boundary". CONFIRMED: that line sits in
the package doc stating "THE BOUNDARY RULE (decision D18). This package does NOT embed a second
executor. DaggerBridge is the WHOLE of crier's execution vocabulary". However the citation is
incomplete as a premise: `internal/harness` ALREADY ships a first-class harness adapter layer
(`harness.Adapter` + `harness.Registry`, spec `CRIER-HARNESS.md`, five adapters), and A2A already
ships as an opt-in protocol (`A2A-OPTION.md` v3, routes gated by `CR_A2A_ENABLED`). The actual gap
is server-mux registration, not harness support.

## 2. Amendments to existing specs (binding reconciliation)

- **A2A-OPTION.md §1 (additive-only, default-off):** unchanged and re-affirmed. The protocol layer
  is a registration refactor only; the A2A two-half gate (`CR_A2A_ENABLED` default false + per-agent
  `a2a` block) is preserved exactly. R-A1: with `CR_A2A_ENABLED` unset, the route census of the
  server is byte-identical to pre-refactor HEAD.
- **CRIER-HARNESS.md:** unchanged. Harness adapters deliver INTO sessions; protocols expose wire
  surfaces. `harness.Registry` is not a `Protocol`; it is consumed by the ingress dispatch hook
  (`cmd/server/harnesswiring.go`). The layer does not wrap it.
- **DETECTION.md / NAMESPACES.md opt-in contract pattern:** the layer copies their fail-closed
  vocabulary — a disabled protocol registers zero routes and logs one line at startup.

## 3. Interface

New package `internal/protocol`. Workers copy these exactly.

```go
// Protocol is one wire surface crier can expose. Implemented by relay, mesh,
// a2a, daggerctl and federation in the first wave.
type Protocol interface {
	// Name is the closed-set protocol id.
	Name() string
	// Enabled reports the protocol's own gate (its existing config field).
	Enabled() bool
	// RegisterRoutes mounts every route the protocol owns onto r.
	// It is called once at startup, only when Enabled() is true.
	// It must not start goroutines, dial peers, or read env directly.
	RegisterRoutes(r *mux.Router, deps Deps) error
}

// Deps carries the shared wiring a protocol may touch. Fields are the same
// values runWithEnv passes today; a protocol that needs a new dep adds the
// field here and nowhere else.
type Deps struct {
	RegistryHandler *registry.Handler
	Store           registry.Store
	Relay           *relay.Relay
	Config          *config.Config
	Logger          *slog.Logger
}

// KnownProtocols is the closed name set, imitating harness.KnownAdapters
// (internal/harness/config.go:38). Unknown names are a load-time hard failure.
var KnownProtocols = []string{"relay", "mesh", "a2a", "dagger", "federation"}
```

Registration loop (replaces the ad-hoc `r.HandleFunc` calls for these five surfaces in
`cmd/server/main.go`; core surfaces — registry/inbox routes `main.go:104-119` and ingress
`POST /ingress/messages` — are NOT protocols and stay direct, decision D-P1: the core delivery
path is not a removable surface):

```go
// cmd/server/protocols.go
func registerProtocols(r *mux.Router, deps protocol.Deps, getenv config.Getenv) error {
	for _, p := range protocol.BuildAll(deps, getenv) { // validated closed set
		if !p.Enabled() {
			slog.Info("protocol disabled", "protocol", p.Name())
			continue
		}
		if err := p.RegisterRoutes(r, deps); err != nil {
			return fmt.Errorf("protocol %s: %w", p.Name(), err)
		}
	}
	return nil
}
```

Each protocol's `Enabled()` reads its existing gate verbatim: relay `true` (always on today),
mesh `true` (always on today), a2a `cfg.A2AEnabled` (`config/config.go:262`), dagger
`cfg.Dagger.URL != ""` (`cmd/server/daggerctl.go:121-123`), federation
`len(cfg.Federation.Links) > 0 || cfg.Federation.RecordsFile != ""` (`main.go:706-762`).

## 4. Wiring

- CLI flags: none new.
- Env vars: none new. Every protocol keeps its existing gate and config fields; the layer adds
  zero config surface.
- HTTP routes: identical to today per the census below.
- main.go: `runWithEnv` builds `protocol.Deps`, calls `registerProtocols(r, deps, getenv)`
  after `registerRegistryRoutes` and before the observability mount (`main.go:1214-1240`).
- Order of registration within the loop is the `KnownProtocols` slice order; gorilla/mux matches
  by path specificity so order does not change routing, but the census test pins it anyway.

## 5. Error Catalog

| Condition | Caller sees |
|---|---|
| Unknown protocol name in `BuildAll` input | startup exits 1 (load-time hard failure, like `harness.Config.Validate`) |
| `RegisterRoutes` returns error | startup exits 1 — partial mux is never served |
| Protocol disabled | zero routes registered, one `slog.Info` line, no error |
| Duplicate `Name()` registered | startup exits 1 (programmer error, detected in `BuildAll`) |

## 6. Edge Cases

- Empty `KnownProtocols` mutation: `BuildAll` returns a fresh slice; callers cannot mutate the
  package var.
- A protocol panics inside `RegisterRoutes`: not recovered — startup fails loudly (same posture
  as today's direct registration).
- Nil `Deps.Relay` with relay enabled: relay's `RegisterRoutes` returns an error → startup
  refusal, not a nil-pointer panic mid-request.
- Concurrent route registration does not exist: `registerProtocols` runs before `http.Serve`.

## 7. Testing

1. **Route-census parity (the gate):** a test that walks the mux before and after the refactor
   for every gate combination of `{a2a on/off} × {dagger url set/unset} × {fed links set/unset}`
   and asserts the set of `(method, path)` pairs is byte-identical to the committed golden census
   taken at pre-refactor HEAD.
2. Test that `CR_A2A_ENABLED=false` yields zero A2A routes (re-affirms A2A-OPTION §1 after the refactor).
3. Test that registering a protocol named `"bogus"` exits with an unknown-protocol error.
4. Test that a protocol whose `RegisterRoutes` returns an error prevents server start.
5. Test that adding a new throwaway protocol implementing `Protocol` requires no edit to
   `cmd/server/main.go` (only `BuildAll`).

## 8. Coverage table

| R# | Statement | Assertion |
|---|---|---|
| R-A1 | Disabled A2A = byte-identical census | test: census parity, a2a off |
| R-A2 | Closed name set, unknown refused at load | test: bogus protocol exits 1 |
| R-A3 | Registration error blocks startup | test: error protocol blocks start |
| R-A4 | New protocol needs no main.go edit | test: throwaway protocol via BuildAll only |
| R-A5 | Disabled protocol = zero routes + one log line | test: census parity per gate combo |

Out of scope (binding): gRPC or REST A2A bindings (A2A-OPTION §7 still binding); wrapping
`harness.Registry` as a Protocol; MCP transport changes (stays a separate stdio binary);
making relay or mesh gateable.
