# DF-CRIER-445 verification

Branch: `wt/DF-CRIER-445`. Base: `4380fec46324bd54f22b0a2f72d2a9b3cb3ba10d`.

Scope: documentation, executable claims and test-only fixtures. No production Go behavior changes, board edits, push or merge.

## Source evidence (commands and actual output)

```sh
git grep -n -E 'HandleFunc\("/(principals|bindings|grants|trust/anchors)"' -- cmd/server/main.go
```

```text
cmd/server/main.go:785:	r.HandleFunc("/trust/anchors", trustHandler.HandleCreateAnchor).Methods("POST")
cmd/server/main.go:786:	r.HandleFunc("/trust/anchors", trustHandler.HandleListAnchors).Methods("GET")
cmd/server/main.go:791:	r.HandleFunc("/principals", registryHandler.HandleListPrincipals).Methods("GET")
cmd/server/main.go:792:	r.HandleFunc("/bindings", registryHandler.HandleListBindings).Methods("GET")
cmd/server/main.go:793:	r.HandleFunc("/grants", registryHandler.HandleListGrants).Methods("GET")
cmd/server/main.go:795:	r.HandleFunc("/principals", registryHandler.HandleMintPrincipal).Methods("POST")
cmd/server/main.go:796:	r.HandleFunc("/bindings", registryHandler.HandleCreateBinding).Methods("POST")
cmd/server/main.go:797:	r.HandleFunc("/grants", registryHandler.HandleCreateGrant).Methods("POST")
```

```sh
git grep -n -E 'managementList\("management-(principals|bindings|grants|peers|anchors)"' -- cmd/server/chat.html
```

```text
cmd/server/chat.html:438:    managementList("management-principals","/principals","items",["id","kind","display_name","namespace","role","status","created_at"],"No principals returned."),
cmd/server/chat.html:439:    managementList("management-bindings","/bindings","items",["id","kind","principal","agent","as_agent","created_at","created_by"],"No bindings returned."),
cmd/server/chat.html:440:    managementList("management-grants","/grants","items",["id","kind","principal","subject","actions","granted_by","granted_at","expires_at"],"No current grants returned."),
cmd/server/chat.html:441:    managementList("management-peers","/fed/peers","peers",["id","name","label","url","state"],"No federation peers returned (0 peers)."),
cmd/server/chat.html:442:    managementList("management-anchors","/trust/anchors","anchors",["id","label","scope","created_at"],"No trust anchors returned."),
```

```sh
git grep -n -E 'func .*Handle(ListPrincipals|ListBindings|ListGrants|MintPrincipal|CreateBinding|CreateGrant)' -- internal/registry/principals_api.go internal/registry/permissions_read.go
```

```text
internal/registry/permissions_read.go:53:func (h *Handler) HandleListPrincipals(w http.ResponseWriter, r *http.Request) {
internal/registry/permissions_read.go:71:func (h *Handler) HandleListBindings(w http.ResponseWriter, r *http.Request) {
internal/registry/permissions_read.go:99:func (h *Handler) HandleListGrants(w http.ResponseWriter, r *http.Request) {
internal/registry/principals_api.go:133:func (h *Handler) HandleMintPrincipal(w http.ResponseWriter, r *http.Request) {
internal/registry/principals_api.go:197:func (h *Handler) HandleCreateBinding(w http.ResponseWriter, r *http.Request) {
internal/registry/principals_api.go:272:func (h *Handler) HandleCreateGrant(w http.ResponseWriter, r *http.Request) {
```

```sh
git grep -n -E 'RegisterAuditRoutes|HandleFunc\("/groups' -- cmd/server/main.go
```

```text
cmd/server/main.go:887:	r.HandleFunc("/groups", sessHandler.HandleCreateGroup).Methods("POST")
cmd/server/main.go:888:	r.HandleFunc("/groups", sessHandler.HandleListGroups).Methods("GET")
cmd/server/main.go:889:	r.HandleFunc("/groups/{name}", sessHandler.HandleGetGroup).Methods("GET")
cmd/server/main.go:890:	r.HandleFunc("/groups/{name}/members", sessHandler.HandleUpdateGroupMembers).Methods("PATCH")
cmd/server/main.go:897:	session.RegisterAuditRoutes(r, sessHandler)
```

```sh
git grep -n -E 'HandleFunc\("/sessions/\{id\}/(audit|permissions|roles)' -- internal/session/audit_http.go
```

```text
internal/session/audit_http.go:261:	r.HandleFunc("/sessions/{id}/audit", h.HandleSessionAudit).Methods(http.MethodGet)
internal/session/audit_http.go:262:	r.HandleFunc("/sessions/{id}/permissions", h.HandleSessionPermissions).Methods(http.MethodGet)
internal/session/audit_http.go:263:	r.HandleFunc("/sessions/{id}/roles", h.HandleSessionRoles).Methods(http.MethodGet)
```

## Real in-process requests

Command:

```sh
GOMAXPROCS=4 GOFLAGS=-p=2 go test -short -count=1 -v -run '^TestDocsClaimsInterfaceAuthorityFixture$' ./cmd/server
```

Actual fixture output (successful request assertions):

```text
POST /principals -> 201; principal stored with role=owner
POST /bindings -> 201; record stored
POST /grants -> 201; record stored
GET /principals -> 200; count=1; minted principal matched
GET /principals with message credential -> 403 MANAGEMENT_FORBIDDEN
GET /bindings -> 200; count=1; minted principal matched
GET /bindings with message credential -> 403 MANAGEMENT_FORBIDDEN
GET /grants -> 200; count=1; minted principal matched
GET /grants with message credential -> 403 MANAGEMENT_FORBIDDEN
GET /fed/peers -> 200
GET /trust/anchors -> 200
```

The fixture uses the real run() server and temporary permission/session/group stores. POSTs use the test admin credential; each list must contain the minted principal or its binding/grant, not merely any record. Federation discovery uses the message credential; trust reads use the admin credential. The first fixture run incorrectly used admin on federation discovery and received the real 403; only the fixture was corrected.

## Claim entries added

14 entries: 9 live route claims and 5 exact-text open-gap anchors. All have `xfail: null`.

```yaml
claims:
- id: ROUTE-CHAT-INTERFACE-PRINCIPALS
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**Principal, binding and grant management ships.**'
  expect: /principals
  xfail: null
- id: ROUTE-CHAT-INTERFACE-BINDINGS
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**Principal, binding and grant management ships.**'
  expect: /bindings
  xfail: null
- id: ROUTE-CHAT-INTERFACE-GRANTS
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**Principal, binding and grant management ships.**'
  expect: /grants
  xfail: null
- id: ROUTE-CHAT-INTERFACE-TRUST-ANCHORS
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '`GET /trust/anchors` and `POST /trust/anchors` also ship'
  expect: /trust/anchors
  xfail: null
- id: ROUTE-CHAT-INTERFACE-AUTHORITY-PEERS
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**The browser reads the authority records (DF-CRIER-384).**'
  expect: /fed/peers
  xfail: null
- id: ROUTE-CHAT-INTERFACE-SESSION-MATRIX
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**A session permission matrix and roles read already ship**'
  expect: /sessions/{id}/permissions
  xfail: null
- id: ROUTE-CHAT-INTERFACE-SESSION-ROLES
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**A session permission matrix and roles read already ship**'
  expect: /sessions/{id}/roles
  xfail: null
- id: ROUTE-CHAT-INTERFACE-SESSION-AUDIT
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**A session permission matrix and roles read already ship**'
  expect: /sessions/{id}/audit
  xfail: null
- id: ROUTE-CHAT-INTERFACE-GROUPS
  kind: route
  doc: specs/CHAT-INTERFACE.md
  quote: '**Named-group APIs ship; the browser roster surface does not.**'
  expect: /groups
  xfail: null
- id: ANCHOR-CHAT-INTERFACE-SIGN-IN-OPEN
  kind: anchor
  doc: specs/CHAT-INTERFACE.md
  quote: '**Browser sign-in is unresolved (DF-CRIER-333).**'
  xfail: null
- id: ANCHOR-CHAT-INTERFACE-LIFECYCLE-ACL-OPEN
  kind: anchor
  doc: specs/CHAT-INTERFACE.md
  quote: '**Close/Reopen cannot satisfy an armed ACL from this page (DF-CRIER-460).**'
  xfail: null
- id: ANCHOR-CHAT-INTERFACE-DEPTH-OPEN
  kind: anchor
  doc: specs/CHAT-INTERFACE.md
  quote: '**The embedded transcript has one-level reply presentation (DF-CRIER-448).**'
  xfail: null
- id: ANCHOR-CHAT-INTERFACE-GROUP-UI-OPEN
  kind: anchor
  doc: specs/CHAT-INTERFACE.md
  quote: '**Named-group rosters are not manageable in the console (DF-CRIER-449).**'
  xfail: null
- id: ANCHOR-CHAT-INTERFACE-ACCESSIBILITY-OPEN
  kind: anchor
  doc: specs/CHAT-INTERFACE.md
  quote: '**No accessibility audit is recorded (DF-CRIER-450).**'
  xfail: null
```

The existing kind=route runner GET-probes for non-404, including named 403 refusals. It cannot express a successful authorized POST or parameterized credential in YAML alone. The TestDocsClaims-prefixed fixture supplies populated GET, successful POST and forbidden-message-credential assertions under the unchanged Makefile selector. No claim language, probe verdict or production route was changed. The existing server fixture now has explicit temporary session/group roots and resource seeds: a named group and a plain message matching the scanner placeholder. Expand therefore answers its real 400 NOT_COMPILED, not resource-not-found 404; that seed is not a compile-success claim.

Adding this spec to the claim set scans the whole document. Dotted /status.field notation was clarified to route plus JSON field; /channels is ignored only because the spec explicitly states that this alias is absent and not owed. No real registered group/message path was ignored.

## Gate

Command:

```sh
GOMAXPROCS=4 GOFLAGS=-p=2 make docs-check
```

Actual output, exit 0:

```text
go test -short -count=1 -run 'TestDocsClaims' ./cmd/server
ok  	github.com/crier-dev/crier/cmd/server	116.420s
```

Build and vet also exited 0:

```sh
GOMAXPROCS=4 GOFLAGS=-p=2 go build ./...
GOMAXPROCS=4 GOFLAGS=-p=2 go vet ./...
```

## Negative controls

Both controls restored the exact original file bytes in a finally block.

1. Temporarily replace the shipped-principal sentence with the old absence assertion; run `go test -short -count=1 -run '^TestDocsClaims$/anchors' ./cmd/server`: exit 1, all three principal/binding/grant claims report `anchor missing`.
2. Temporarily change the principal route claim expect to `/df-crier-445-not-a-route`; run `go test -short -count=1 -run '^TestDocsClaims$/routes' ./cmd/server`: exit 1, `ROUTE-CHAT-INTERFACE-PRINCIPALS` reports `expected=non-404 observed=404`.

Logs: `/tmp/DF-CRIER-445-negative-anchor.log` and `/tmp/DF-CRIER-445-negative-route.log`.

## Findings and verification limits

- The minimum brief understated two more backend surfaces: session permissions/roles/audit and named-group CRUD already ship. Existing `TestPermissionMatrixPerBackend` (JSONL + SQLite), `TestGroupAPIRoutesServed`, `TestManagementRoutesWithDualTokens` and `TestChatManagementRoutesInServedScript` all passed before documentation edits. Consequently no absence of a per-principal matrix or named-group API is claimed. The missing group surface is the browser roster workflow.
- The session matrix is membership-role/grant presentation, not proof of effective delivery authority. It has four action columns and no per-session mint operation; it is not the reference image’s full matrix editor.
- Browser sign-in DF-CRIER-333, armed-ACL Close/Reopen DF-CRIER-460, nested UI navigation DF-CRIER-448, console rosters DF-CRIER-449 and accessibility audit DF-CRIER-450 remain explicitly open. No fix for those rows is included.
- Browser rendering was checked in source and the existing served-script test, not re-audited in a live browser. The previously reported live “Populated” browser observation was not independently repeated; the real in-process HTTP fixture demonstrates the underlying populated reads.
- No new accessibility audit or exhaustive historical audit search was performed. The status is grounded in the open coordinator row, not a new claim of compliance.
- No PostgreSQL instance, external federation peer, external identity provider or production deployment was exercised by this task.

## Full short-suite verification

Command, exit 0:

```sh
GOMAXPROCS=4 GOFLAGS=-p=2 go test -short -count=1 -parallel 4 -timeout 180s ./...
```

Actual output:

```text
ok  	github.com/crier-dev/crier/cmd/crier-mcp	3.098s
ok  	github.com/crier-dev/crier/cmd/server	172.202s
ok  	github.com/crier-dev/crier/config	0.597s
?   	github.com/crier-dev/crier/examples/sqlite-quickstart	[no test files]
ok  	github.com/crier-dev/crier/examples/ws-mesh-demo	10.356s
ok  	github.com/crier-dev/crier/internal/a2a	0.007s
ok  	github.com/crier-dev/crier/internal/addressing	0.003s
ok  	github.com/crier-dev/crier/internal/buildinfo	0.002s
ok  	github.com/crier-dev/crier/internal/chat	0.002s
ok  	github.com/crier-dev/crier/internal/ci	0.005s
ok  	github.com/crier-dev/crier/internal/daggerctl	2.533s
ok  	github.com/crier-dev/crier/internal/detect	0.149s
ok  	github.com/crier-dev/crier/internal/dispatchlive	0.004s
ok  	github.com/crier-dev/crier/internal/federation	2.166s
ok  	github.com/crier-dev/crier/internal/guard	15.302s
ok  	github.com/crier-dev/crier/internal/httperr	0.002s
ok  	github.com/crier-dev/crier/internal/mcp	6.777s
ok  	github.com/crier-dev/crier/internal/mesh	7.372s
ok  	github.com/crier-dev/crier/internal/metrics	0.009s
ok  	github.com/crier-dev/crier/internal/middleware	0.008s
ok  	github.com/crier-dev/crier/internal/namespace	0.003s
ok  	github.com/crier-dev/crier/internal/permissions	0.540s
ok  	github.com/crier-dev/crier/internal/pidfile	0.108s
ok  	github.com/crier-dev/crier/internal/ratelimit	0.605s
ok  	github.com/crier-dev/crier/internal/registry	41.284s
ok  	github.com/crier-dev/crier/internal/relay	7.331s
ok  	github.com/crier-dev/crier/internal/session	65.516s
ok  	github.com/crier-dev/crier/internal/strictjson	0.003s
ok  	github.com/crier-dev/crier/internal/testsupport	0.661s
ok  	github.com/crier-dev/crier/internal/trust	0.010s
ok  	github.com/crier-dev/crier/internal/webhook	16.651s
```
