#!/usr/bin/env bash
#
# scripts/check-demo-cleanup.sh — the server-spawn cleanup-contract gate
# (CR-GAP-069).
#
# WHY THIS EXISTS
# ---------------
# The dogfood federation demo leaked two orphan crier servers on :18767/:18877
# (reaped by hand). The contract that prevents it lives in the demo harness —
# scripts/e2e-battery.sh is the reference: a script that SPAWNS a crier server
#    (a) registers an EXIT trap that kills the server it started, and
#    (b) asserts, after the start, that the pid HOLDING the port is the pid it
#        started (assert_port_owned / port_holder_pid from scripts/lib/port-guard.sh,
#        or the equivalent `ss -tlnp` holder-pid comparison),
# so the process cannot outlive the script and a squatter cannot be mistaken for it.
#
# Every example runner and e2e-battery.sh already satisfy that contract. NOTHING
# enforced it: an ad-hoc dogfood script (or a future runner) that backgrounds
# ./bin/crier with no trap re-created the leak with no signal — which is exactly
# what happened. This checker is the arm that gives that mistake a red instead.
#
# It follows the repo's established checker pattern (DF-CRIER-206 shell-yaml,
# DF-CRIER-209 make-docker, DF-CRIER-189 gofmt): static analysis, every rejection
# names file:line, an EXPLICIT file list fails closed, and the default mode checks
# the tracked tree.
#
# WHAT IS CHECKED
# ---------------
# Scope (default mode, no arguments): every TRACKED shell script — `git ls-files
# '*.sh'` — PLUS tracked shell runners regardless of extension (a tracked file
# under examples/ whose first line is a shell shebang, and any tracked path whose
# name matches *run-demo*). An empty scope is refused (exit 2): a green over zero
# files is never printed.
#
# Classification — a script is SERVER-SPAWNING when its text actually SPAWNS a
# crier server binary (the relay `bin/crier` / `cmd/server`, or the MCP server
# `bin/crier-mcp` / `cmd/crier-mcp`), in one of these executable forms:
#     * a backgrounded invocation            ./bin/crier > "$WORKDIR/server.log" 2>&1 &
#     * a wrapped invocation                 env -i PATH=… CRIER_PORT=… "$REPO/bin/crier" … &
#     * a timeout-bounded invocation         timeout 30 "$CRIER_BIN" -port "$PORT" … &
#     * a shell-string invocation            bash -c './bin/crier --port 9001 &'
#     * a launcher run under a bounded shell timeout -k 5 "$BUDGET" bash -c "$launcher"
#     * `go run ./cmd/server` / `make run` (backgrounded) / `make mcp`
# A mere MENTION never classifies. Suppressed contexts are: comments; heredoc
# bodies (usage text, transcripts, inline python); the message commands
# (echo/printf/fatal/fail/ok/…) — which is what scripts/bunker-matrix.sh does with
# `./bin/crier` and `make run`; and the non-executing commands (grep/test/awk/
# sed/head/…), which is what scripts/bunker-matrix-selftest.sh does with
# `grep -q "make run"`. `go build -o … ./cmd/server` is a BUILD, not a spawn, and
# the binary's own meta/control flags (`-version`, `-help`, `-stop`) are not a
# spawn either — they answer or stop, they never start a listener. The same goes
# for the `keygen` subcommand (CR-FEAT-027): `crier keygen` writes a keypair and
# prints an agent config, and binds no port, so a script whose only crier
# invocation is `crier keygen` owes neither (a) nor (b).
#
# A classified script is COMPLIANT iff BOTH requirements hold:
#     (a) EXIT-trap cleanup — an EXIT trap whose handler (an inline trap command
#         OR the cleanup function the trap names) contains a `kill` that
#         references the pid variable of the spawned server (the `VAR=$!` that
#         follows the spawn) — i.e. the server cannot outlive the script.
#         (a) is owed only when the script BACKGROUNDS a server: a foreground run
#         cannot outlive the script, and a `timeout`-wrapped site is reaped by its
#         own timeout.
#     (b) ownership — an after-start assertion that the port is held by the pid we
#         started: a call to `assert_port_owned` or `port_holder_pid`, or an
#         `ss -tln…` holder-pid comparison that names the started pid.
#     (b) is required only when the script BINDS A PORT: a relay spawn always
#     carries one (the relay listens), so every relay spawner owes it; an
#     MCP/stdio-only launcher binds nothing and owes none.
#
# EXCEPTION — BOUNDED EXECUTION. A script whose crier server cannot outlive it
# because the whole run is wall-clock bounded satisfies (a) WITHOUT a trap:
#     (i)  every spawn site is wrapped by `timeout` on its own line, or
#     (ii) the script has no backgrounded spawn site and launches through a
#          bounded executor — a `timeout … bash -c` invocation (the pattern
#          scripts/check-mcp-stdout.sh uses: timeout -k 5 is the reaping
#          mechanism, so no trap is needed and no trap is written).
# A bounded-execution script is exempt from (a); it still owes (b) when it binds
# a port.
#
# OUTPUT
# ------
# One line per script — NOT-SPAWNING / COMPLIANT / REJECTED — plus, for every
# rejection, the spawn line that caused classification and each missing
# requirement ((a) and/or (b)) by name, with file:line.
#
# FAIL CLOSED
# -----------
#   * explicit list: a named path that does not exist, a named path that is not a
#     shell script, or a list in which NOTHING classifies as server-spawning,
#     exits 1 with the offending paths named (never a vacuous PASS);
#   * default mode: an empty scope exits 2 (nothing was verified);
#   * missing tool (git for the default scope, python3 for the analyzer) exits 2
#     naming the tool — never a silent skip.
#
# KNOWN LIMITS (static text analysis — stated, not implied)
#   * it reads TEXT, so it cannot follow a path built at run time (a launcher held
#     in a variable it cannot resolve, a `go run` of a package alias), and the
#     `bash -c '<cmd>'` recursion is ONE level deep;
#   * classification requires an executable spawn form — a script that only
#     mentions the binary is out of scope by design (and is not a leak); the
#     binary's own meta/control flags (`-version`, `-help`, `-stop`) are treated as
#     non-spawns, because they answer or stop rather than start a listener;
#   * bounded-execution equivalence is FORM-based: `timeout` on the spawn (or a
#     `timeout … bash -c` executor) is accepted as the reaping mechanism even
#     though what makes it true is that the timeout actually fires;
#   * requirement (a) is owed only by a BACKGROUNDED spawn: a foreground run cannot
#     outlive the script, so a script that always runs the server in the foreground
#     needs no trap (it still owes (b) when it binds a port);
#   * an MCP/stdio launcher is classified as a crier server spawn but owes no
#     ownership assertion (it binds no port);
#   * a crier server started as a CONTAINER (`docker run … crier:test`, the
#     scripts/bunker-deploy.sh shape) is outside this gate: its lifecycle belongs
#     to docker, not to the script's process table;
#   * ordering is not proven — (b) is required to EXIST, its position relative to
#     the start is not asserted, and an EXIT trap is not proven to be registered
#     before the spawn it reaps.
#
# USAGE
#   bash scripts/check-demo-cleanup.sh                     # tracked tree
#   bash scripts/check-demo-cleanup.sh <file> [<file> …]   # exactly those scripts
#   bash scripts/check-demo-cleanup.sh --selftest          # run the selftest
#
# EXIT CODES
#   0  every classified script is compliant
#   1  at least one rejection, or a fail-closed rule tripped
#   2  nothing was verified (missing tool, empty scope)
#
# DEPENDENCIES: bash, python3 (stdlib only — the text analyzer), git (default
# scope only).

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-demo-cleanup"
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
SELFTEST_SCRIPT="$SCRIPT_DIR/check-demo-cleanup-selftest.sh"

# Scope knobs, overridable so the selftest can drive other trees.
SCOPE_ROOT="${DEMO_CLEANUP_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}"

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

usage() {
  cat <<'EOF'
check-demo-cleanup.sh — fail when a tracked shell script spawns a crier server
without the cleanup contract (CR-GAP-069)

Usage:
  bash scripts/check-demo-cleanup.sh                     # tracked tree (default)
  bash scripts/check-demo-cleanup.sh <file> [<file> …]   # exactly those scripts
  bash scripts/check-demo-cleanup.sh --selftest          # prove the checker still works

A script that spawns a crier server must (a) register an EXIT trap that kills the
server it started, and (b) assert after the start that the port's holder pid is the
pid it started (assert_port_owned / port_holder_pid / an ss -tlnp comparison).
A script whose whole run is wall-clock bounded (timeout on the spawn, or a
`timeout … bash -c` launcher executor) is exempt from (a) — the timeout is its
reaping mechanism — but still owes (b) when it binds a port.

Exit codes: 0 all classified scripts compliant, 1 a rejection or a fail-closed
rule, 2 nothing was verified (missing tool, empty scope).
EOF
}

# ── file classification + scope ───────────────────────────────────────────────

_is_shell_script() { # <path> — .sh, or a shell shebang, or a *run-demo* runner
  local f="$1"
  [ -f "$f" ] || return 1
  case "$f" in
    *.sh) return 0 ;;
    *run-demo*) return 0 ;;
  esac
  head -n 1 "$f" 2>/dev/null | grep -Eq '^#!.*(bash|/sh|dash|ksh)' && return 0
  return 1
}

_collect_default_scope() { # prints one path per line, sorted, deduped
  local root="$SCOPE_ROOT"
  command -v git >/dev/null 2>&1 || {
    _err "'git' is not on PATH — the DEFAULT scope is the tracked file set and"
    _err "  cannot be enumerated without it. Pass an explicit file list instead."
    return 2
  }
  [ -d "$root/.git" ] || [ -f "$root/.git" ] || {
    _err "no git repository at $root — the DEFAULT scope is the tracked file set."
    _err "  Set DEMO_CLEANUP_ROOT to the checkout, or pass an explicit file list."
    return 2
  }
  {
    git -C "$root" ls-files '*.sh'
    # "regardless of extension": a tracked runner under examples/, or any tracked
    # *run-demo* path, that carries a shell shebang.
    git -C "$root" ls-files 'examples/**' 'examples/*' 2>/dev/null
    git -C "$root" ls-files '*run-demo*' 2>/dev/null
  } | while IFS= read -r rel; do
        [ -n "$rel" ] || continue
        if _is_shell_script "$root/$rel"; then printf '%s\n' "$rel"; fi
      done | sort -u
  return 0
}

# ── the analyzer ──────────────────────────────────────────────────────────────
#
# Records (tab-separated) on stdout, one per finding:
#   SPAWN   <line> <relay|mcp> <bg|exec|bounded> <pidvar|-> <text>
#   TRAPEXIT <line> <inline|func> <handler-text-or-function-name>
#   TRAPBODY <line> <text>              # body of the function the trap names
#   ASSERT  <line> <text>
#   EXECUTOR <line> <text>              # `timeout … bash -c` bounded executor
_analyze() { # <file>
  python3 - "$1" <<'PYEOF'
import re
import sys

path = sys.argv[1]
try:
    with open(path, 'r', encoding='utf-8', errors='replace') as fh:
        raw = fh.read().splitlines()
except OSError as exc:
    sys.stderr.write("cannot read %s: %s\n" % (path, exc))
    sys.exit(2)

# ── vocabulary ────────────────────────────────────────────────────────────────
# Message commands: the arguments are TEXT. A line whose command is one of these
# is a mention, never a spawn (the bunker-matrix.sh `fatal "… make run, or
# ./bin/crier …"` shape).
MESSAGE_CMDS = {
    'echo', 'printf', 'cat', 'say', 'step', 'pass', 'fail', 'ok', 'bad', 'warn',
    'info', 'note', 'msg', 'log', 'die', 'fatal', 'abort', 'shout', 'usage',
    '_err', '_info', '_ok', '_bad', '_warn', '_log', '_msg', '_die', '_say',
}
# Non-executing commands: these read, test or transform text. A crier path in
# their arguments is data (the `grep -q "make run"` shape).
NONEXEC_CMDS = {
    'grep', 'egrep', 'fgrep', 'rg', 'awk', 'sed', 'cut', 'tr', 'wc', 'head',
    'tail', 'sort', 'uniq', 'tee', 'read', 'readlink', 'realpath', 'basename',
    'dirname', 'test', '[', '[[', 'command', 'type', 'which', 'builtin', 'hash',
    'ls', 'stat', 'file', 'diff', 'cmp', 'find', 'local', 'declare', 'readonly',
    'export', 'unset', 'return', 'shift', 'set', 'trap', 'true', 'false', ':',
}
# Wrapper commands: what may sit BETWEEN the shell and the real command. `env`
# and `timeout` peel their own flags/assignments; `timeout` additionally marks the
# spawn BOUNDED (the exception rule).
WRAPPER_CMDS = {'env', 'timeout', 'nohup', 'setsid', 'exec', 'sudo', 'stdbuf',
                'nice', 'ionice', 'xargs'}
# Shells that can run a command STRING (`bash -c '<cmd>'`) — the string is analyzed
# too, because what it spawns is spawned by this script.
SHELL_WORDS = {'bash', 'sh', 'dash', 'ksh'}
COMPOUND_KW = {'if', 'then', 'elif', 'else', 'do', 'while', 'until', 'for', 'fi',
               'done', 'esac', 'case', 'in', '{', '}', '(', ')', '!', ';;'}

# The relay server binary, in every form the demos use: ./bin/crier, bin/crier,
# $REPO/bin/crier, $WORKDIR/crier, $CRIER_BIN, ./crier.
RELAY_WORD = re.compile(
    r'^(?:(?:.*/)?bin/crier|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?/crier'
    r'|\$\{?CRIER_BIN\}?|(?:\./)?crier)$')
# The MCP server binary (a crier server too — it is what the documented launcher
# runs and what `make mcp` execs).
MCP_WORD = re.compile(
    r'^(?:(?:.*/)?bin/crier-mcp|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?/crier-mcp'
    r'|\$\{?CRIER_MCP_BIN\}?|\$\{?BRIDGE\}?)$')
# `go run ./cmd/server` / `go run ./cmd/crier-mcp` — a run target, not a
# `cmd/server/openapi.yaml` path.
GO_TARGET_RE = re.compile(r'^(?:\./)?cmd/(server|crier-mcp)$')

PORT_RE = re.compile(r'CRIER_PORT|-port[ =]|--port[ =]')
# The binary's OWN meta/control flags: `./bin/crier -version`, `-help`, `-stop`.
# Those print an answer or stop a server through its pidfile — they do not start a
# listener, so such an invocation is not a spawn.
META_FLAG_RE = re.compile(r'(^|[\s"\'])-(version|help|stop|h)([\s"\']|$)'
                          r'|--(version|help)([\s"\']|$)')
# The `keygen` SUBCOMMAND (CR-FEAT-027) is the same class of non-spawn as those
# flags: `crier keygen` generates a keypair, prints the agent config and exits —
# it binds no port and starts no server. Measured before this rule existed: a
# fixture whose ONLY crier invocation was `crier keygen` was classified as a
# relay spawn and REJECTED for missing the (b) ownership assertion, for a port it
# never binds. The rule needs the subcommand word in the argument position
# directly after the binary (the binary spelled literally, or through a variable
# such as "$CRIER_BIN"), so a line that merely mentions "keygen" elsewhere is
# unaffected.
SUBCOMMAND_NO_LISTEN_RE = re.compile(
    r'(?:^|[\s"\'=(])(?:\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|(?:[^\s"\']*/)?crier(?:-mcp)?)'
    r'[\s"\']*keygen(?:[\s"\']|$)')
HEREDOC_RE = re.compile(r'<<-?[ \t]*(["\']?)([A-Za-z_][A-Za-z0-9_]*)\1')
FUNC_RE = re.compile(r'^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)\s*\{')
PIDVAR_RE = re.compile(r'^\s*([A-Za-z_][A-Za-z0-9_]*)=(?:"|\')?\$\{?\!\}?(?:"|\')?\s*$')
ASSIGN_RE = re.compile(r'^[A-Za-z_][A-Za-z0-9_]*\+?=')
EXECUTOR_RE = re.compile(r'\b(bash|sh|dash|ksh)\b[^\n]*\s-c\b')
TIMEOUT_WORD_RE = re.compile(r'^(?:timeout|\$\{?[A-Za-z_]*TIMEOUT[A-Za-z_]*\}?)$')
NUMBERISH_RE = re.compile(r'^[0-9.]+[smhd]?$')


def strip_comment(line):
    out = []
    quote = ''
    i = 0
    while i < len(line):
        ch = line[i]
        if quote:
            if ch == '\\' and quote == '"':
                out.append(ch)
                i += 1
                if i < len(line):
                    out.append(line[i])
                i += 1
                continue
            if ch == quote:
                quote = ''
            out.append(ch)
        else:
            if ch in ('"', "'"):
                quote = ch
                out.append(ch)
            elif ch == '#' and (not out or out[-1] in (' ', '\t')):
                break
            else:
                out.append(ch)
        i += 1
    return ''.join(out)


def norm(word):
    w = word.strip()
    while w and w[0] in '"\'({':
        w = w[1:]
    while w and w[-1] in '"\'()}{;,':
        w = w[:-1]
    return w


def is_relay(word):
    return bool(RELAY_WORD.match(word))


def is_mcp(word):
    return bool(MCP_WORD.match(word))


def is_crier(word):
    return is_relay(word) or is_mcp(word)


# ── pass 1: comment-stripped, continuation-joined logical lines ───────────────
logical = []          # (lineno, text)
functions = {}        # name -> list of (lineno, text) body lines
in_heredoc = False
hterm = ''
pending_term = None
i = 0
while i < len(raw):
    line = raw[i]
    if in_heredoc:
        if line.lstrip('\t').strip() == hterm:
            in_heredoc = False
        i += 1
        continue
    text = strip_comment(line)
    m = HEREDOC_RE.search(text)
    if m:
        pending_term = m.group(2)
    start = i + 1
    while text.rstrip().endswith('\\') and i + 1 < len(raw):
        i += 1
        nxt = strip_comment(raw[i])
        text = text.rstrip()[:-1] + ' ' + nxt
    logical.append((start, text))
    if pending_term is not None:
        in_heredoc = True
        hterm = pending_term
        pending_term = None
    i += 1

# ── pass 2: function bodies (for trap-handler resolution) ─────────────────────
for idx, (lineno, text) in enumerate(logical):
    fm = FUNC_RE.match(text)
    if not fm:
        continue
    name = fm.group(1)
    # the definition line is part of the body — a one-line
    # `cleanup() { kill "$SERVER_PID"; }` is idiomatic and would otherwise
    # contribute no text to the trap-handler check.
    depth = text.count('{') - text.count('}')
    body = [(lineno, text)]
    j = idx + 1
    while depth > 0 and j < len(logical):
        bl, bt = logical[j]
        body.append((bl, bt))
        depth += bt.count('{') - bt.count('}')
        j += 1
    functions.setdefault(name, body)

# ── pass 3: findings ──────────────────────────────────────────────────────────
def peel(tokens, depth=0):
    """Walk a segment towards its real command word.
    Returns (word, bounded, bg) — word is None when the segment runs something
    else. `bg` is True when the spawn is BACKGROUNDED (the segment ends with `&`,
    or an analyzed shell string backgrounds it), `bounded` when a `timeout`
    wrapper reaches it. `depth` bounds the shell-string recursion
    (`bash -c './bin/crier &'`)."""
    idx = 0
    bounded = False
    guard = 0
    while idx < len(tokens) and guard < 64:
        guard += 1
        t = norm(tokens[idx])
        if t in COMPOUND_KW or t == '':
            idx += 1
            continue
        # An assignment is never a spawn — and it must be tested BEFORE the token
        # match, because `bin="$REPO_ROOT/bin/crier-mcp"` ends with the binary path
        # and would otherwise classify as a spawn site.
        if ASSIGN_RE.match(t):
            idx += 1
            continue
        if t in MESSAGE_CMDS or t in NONEXEC_CMDS:
            return None, bounded, False
        if is_crier(t):
            return t, bounded, False
        if t in SHELL_WORDS:
            # a shell running a COMMAND STRING: whatever `bash -c './bin/crier &'`
            # spawns is spawned by this script, so analyze that string too (one
            # level deep; a deeper nesting is reported by its own line, if any).
            # The inner segment's own `&` is propagated — an inner backgrounded
            # spawn is exactly what leaks.
            if '-c' in tokens[idx + 1:] and depth < 1:
                k = tokens.index('-c', idx + 1)
                inner = ' '.join(tokens[k + 1:]).strip()
                if (inner.startswith("'") and inner.endswith("'")) \
                        or (inner.startswith('"') and inner.endswith('"')):
                    inner = inner[1:-1]
                for iseg in re.split(r'&&|\|\||;|\|', inner):
                    itoks = iseg.split()
                    if not itoks:
                        continue
                    if norm(itoks[0]) in MESSAGE_CMDS:
                        continue
                    stripped = iseg.strip()
                    ibg = stripped.endswith('&') and not stripped.endswith('&&')
                    w, b, _ = peel(itoks, depth + 1)
                    if w:
                        return w, (bounded or b), ibg
            return None, bounded, False
        if t in WRAPPER_CMDS or TIMEOUT_WORD_RE.match(t):
            if t == 'timeout' or TIMEOUT_WORD_RE.match(t):
                bounded = True
            idx += 1
            # peel the wrapper's own flags/assignments/budget
            while idx < len(tokens):
                a = norm(tokens[idx])
                if is_crier(a):
                    break
                if a == '-u' or a == '-C' or a == '-f':
                    idx += 2
                    continue
                if a.startswith('-') or NUMBERISH_RE.match(a) or ASSIGN_RE.match(a):
                    idx += 1
                    continue
                break
            continue
        if t == 'make':
            idx += 1
            sub = ''
            while idx < len(tokens):
                a = norm(tokens[idx])
                if a.startswith('-'):
                    idx += 1
                    continue
                if ASSIGN_RE.match(a):
                    idx += 1
                    continue
                sub = a
                break
            if sub == 'run':
                return 'make run', bounded, False
            if sub == 'mcp':
                return 'make mcp', bounded, False
            return None, bounded, False
        if t == 'go':
            idx += 1
            while idx < len(tokens) and norm(tokens[idx]).startswith('-'):
                idx += 1
            if idx < len(tokens) and norm(tokens[idx]) == 'run':
                for a in (norm(x) for x in tokens[idx:]):
                    if GO_TARGET_RE.match(a):
                        return 'go run ' + a, bounded, False
            return None, bounded, False
        return None, bounded, False
    return None, bounded, False


findings = []
for lidx, (lineno, text) in enumerate(logical):
    body = text.strip()
    if not body:
        continue
    segs = re.split(r'&&|\|\||;|\|', text)
    first_tok = norm(segs[0].split()[0]) if segs and segs[0].split() else ''
    whole_line_is_message = first_tok in MESSAGE_CMDS
    for seg in segs:
        toks = seg.split()
        if not toks:
            continue
        if whole_line_is_message:
            continue
        if norm(toks[0]) in MESSAGE_CMDS:
            continue
        word, bounded, inner_bg = peel(toks)
        if word is None:
            continue
        # the binary's own meta/control flags, and its non-listening `keygen`
        # subcommand, are not spawns (see META_FLAG_RE / SUBCOMMAND_NO_LISTEN_RE)
        if META_FLAG_RE.search(text) or SUBCOMMAND_NO_LISTEN_RE.search(text):
            continue
        seg_stripped = seg.strip()
        bg = (seg_stripped.endswith('&') and not seg_stripped.endswith('&&')) \
            or inner_bg
        kind = 'mcp' if (word in ('make mcp',) or is_mcp(word)) else 'relay'
        if word == 'make run' and not bg:
            continue        # `make run` only counts backgrounded
        # the spawn's pid variable is the `VAR=$!` that follows it — looked up in
        # the LOGICAL stream, because continuations / trailing comments mean the
        # raw line numbers after a spawn are not the next entry.
        pidvar = '-'
        for k in range(1, 4):
            if lidx + k >= len(logical):
                break
            pm = PIDVAR_RE.match(strip_comment(logical[lidx + k][1]))
            if pm:
                pidvar = pm.group(1)
                break
        if pidvar == '-':
            pm = PIDVAR_RE.match(strip_comment(text))
            if pm:
                pidvar = pm.group(1)
        mode = 'bounded' if bounded else ('bg' if bg else 'exec')
        findings.append(('SPAWN', lineno, kind, mode, pidvar,
                         text.strip()[:200]))

# trap on EXIT + its handler
for lineno, text in logical:
    m = re.search(r'\btrap\s+(.+?)\s+EXIT\b', text)
    if not m:
        continue
    handler = m.group(1).strip()
    hnorm = norm(handler)
    if hnorm == '-':
        continue
    if hnorm in functions:
        findings.append(('TRAPEXIT', lineno, 'func', hnorm))
        for bl, bt in functions[hnorm]:
            findings.append(('TRAPBODY', bl, bt.strip()[:200]))
    else:
        findings.append(('TRAPEXIT', lineno, 'inline', handler.strip('"\'')))

# after-start ownership assertion
for lineno, text in logical:
    if not text.strip():
        continue
    has_guard = re.search(r'\b(assert_port_owned|port_holder_pid)\b', text)
    if has_guard:
        toks = text.split()
        if toks and norm(toks[0]) in MESSAGE_CMDS:
            continue
        findings.append(('ASSERT', lineno, text.strip()[:200]))
        continue
    if re.search(r'\bss\s+-tln', text) and re.search(r'pid=\$?\{?[A-Za-z_]', text):
        findings.append(('ASSERT', lineno, text.strip()[:200]))

# bounded executor: `timeout … bash -c <cmd>` — the launcher pattern whose
# timeout/kill-after is the reaping mechanism (so no trap is needed).
for lineno, text in logical:
    toks = [norm(t) for t in text.split()]
    if any(TIMEOUT_WORD_RE.match(t) for t in toks) and EXECUTOR_RE.search(text):
        findings.append(('EXECUTOR', lineno, text.strip()[:200]))

for f in findings:
    sys.stdout.write('\t'.join(str(x) for x in f) + '\n')
PYEOF
}

# ── per-file verdict ──────────────────────────────────────────────────────────
#
# The analyzer output is computed ONCE per file and cached here, so the caller can
# CLASSIFY (a fact about the text) independently of the verdict (a judgement about
# the contract). The selftest's NEUTER proof depends on that separation: disabling
# the verdict must not also disable classification, or the proof would report the
# "nothing was enforced" rule instead of the verdict under test.
_CLASSIFY_FILE=""
_CLASSIFY_RECS=""

_analyze_cached() { # <file>
  if [ "$_CLASSIFY_FILE" = "$1" ] && [ -n "$_CLASSIFY_RECS" ]; then
    printf '%s\n' "$_CLASSIFY_RECS"
    return 0
  fi
  _CLASSIFY_RECS="$(_analyze "$1")" || {
    _CLASSIFY_RECS=""
    return 2
  }
  _CLASSIFY_FILE="$1"
  printf '%s\n' "$_CLASSIFY_RECS"
  return 0
}

_classify() { # <file> — sets _CHECK_CLASS to spawn|no|error
  local recs=""
  _CHECK_CLASS="no"
  recs="$(_analyze_cached "$1")" || {
    _CHECK_CLASS="error"
    return 2
  }
  if printf '%s\n' "$recs" | grep -q $'^SPAWN\t'; then
    _CHECK_CLASS="spawn"
  fi
  return 0
}

# NEUTER-MARK[verdict]: the rejection verdict call lives in run_check's loop, so
# the selftest can force it to success and prove the rejection is caused by this
# analysis and nothing else.
_check_script() { # <file>; prints the file's analysis + verdict, returns 1 on rejection
  local f="$1"
  local recs="" line="" rc=0
  local -a spawns=() pidvars=() traps=() trapbodies=() asserts=() executors=()
  local -a missing=()

  recs="$(_analyze_cached "$f")" || {
    _err "analysis of $f failed — nothing was verified for it."
    return 2
  }

  local -a F=()
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    IFS=$'\t' read -r -a F <<<"$line"
    case "${F[0]}" in
      SPAWN)    spawns+=("$line") ;;
      TRAPEXIT) traps+=("$line") ;;
      TRAPBODY) trapbodies+=("$line") ;;
      ASSERT)   asserts+=("$line") ;;
      EXECUTOR) executors+=("$line") ;;
    esac
  done <<<"$recs"

  if [ "${#spawns[@]}" -eq 0 ]; then
    printf 'NOT-SPAWNING  %s — no crier server spawn site (mentions only)\n' "$f"
    return 0
  fi

  # spawn sites, for the reader: what classified this file, and where.
  local s="" sl="" sk="" sm="" sp="" st=""
  local bg_sites=0 relay_sites=0 all_bounded=1
  for s in "${spawns[@]}"; do
    IFS=$'\t' read -r -a F <<<"$s"
    sl="${F[1]}"; sk="${F[2]}"; sm="${F[3]}"; sp="${F[4]}"; st="${F[5]}"
    printf '  spawn site  %s:%s  [%s/%s]  pidvar=%s\n                %s\n' \
      "$f" "$sl" "$sk" "$sm" "$sp" "$st"
    [ "$sk" = relay ] && relay_sites=$((relay_sites + 1))
    if [ "$sm" = bg ]; then bg_sites=$((bg_sites + 1)); fi
    [ "$sm" = bounded ] || all_bounded=0
    [ "$sp" != "-" ] && pidvars+=("$sp")
  done

  # (a) EXIT trap that kills the server it started.
  local trap_ok=0 handler="" hl="" hkind="" htext="" pv=""
  if [ "${#traps[@]}" -eq 0 ]; then
    handler=""
  else
    IFS=$'\t' read -r -a F <<<"${traps[0]}"
    hl="${F[1]}"; hkind="${F[2]}"; htext="${F[3]}"
    handler="$htext"
  fi
  local body=""
  for st in "${trapbodies[@]}"; do
    IFS=$'\t' read -r -a F <<<"$st"
    body="$body ${F[2]}"
  done
  local handler_all="$handler $body"
  if [ "${#traps[@]}" -gt 0 ]; then
    if printf '%s' "$handler_all" | grep -q 'kill'; then
      trap_ok=1
      if [ "${#pidvars[@]}" -gt 0 ]; then
        for pv in $(printf '%s\n' "${pidvars[@]}" | sort -u); do
          if ! printf '%s' "$handler_all" | grep -q "\$${pv}\|\${${pv}}"; then
            trap_ok=0
            break
          fi
        done
      fi
    fi
  fi

  # bounded-execution exception (a) is waived
  local bounded_ok=0 bounded_why=""
  if [ "$all_bounded" -eq 1 ]; then
    bounded_ok=1
    bounded_why="every spawn site is wrapped by timeout"
  elif [ "$bg_sites" -eq 0 ] && [ "${#executors[@]}" -gt 0 ]; then
    bounded_ok=1
    IFS=$'\t' read -r -a F <<<"${executors[0]}"
    bounded_why="no backgrounded spawn site; launched through a bounded executor (timeout … bash -c at :${F[1]})"
  fi

  local owners=0 assert_line=""
  if [ "${#asserts[@]}" -gt 0 ]; then
    owners=1
    IFS=$'\t' read -r -a F <<<"${asserts[0]}"
    assert_line="${F[1]}"
  fi

  if [ "$trap_ok" -eq 0 ] && [ "$bg_sites" -gt 0 ]; then
    missing+=("(a) EXIT-trap cleanup: no \`trap … EXIT\` that kills the spawned server pid (need a kill of ${pidvars[*]:-the started pid} in the trapped command or the cleanup function it calls)")
  fi
  if [ "$owners" -eq 0 ] && [ "$relay_sites" -gt 0 ]; then
    missing+=("(b) ownership assertion: after starting the relay, nothing asserts that the port's holder pid is the pid that was started (assert_port_owned / port_holder_pid / an ss -tlnp holder-pid comparison)")
  fi

  if [ "${#missing[@]}" -eq 0 ]; then
    local why=""
    if [ "$trap_ok" -eq 1 ]; then
      why="(a) EXIT trap ${hkind} at :${hl} kills ${pidvars[*]:-the started pid}"
    elif [ "$bounded_ok" -eq 1 ]; then
      why="(a) waived — bounded execution (${bounded_why})"
    else
      why="(a) n/a — no backgrounded spawn site (a foreground run cannot outlive the script)"
    fi
    local whyb
    if [ "$owners" -eq 1 ]; then whyb="(b) ownership asserted at :${assert_line}"; else whyb="(b) n/a — no relay port bound"; fi
    printf 'COMPLIANT     %s — %s spawn site(s): %s; %s\n' "$f" "${#spawns[@]}" "$why" "$whyb"
    return 0
  fi

  printf 'REJECTED      %s\n' "$f"
  local m=""
  for m in "${missing[@]}"; do
    printf '              missing %s\n' "$m"
  done
  printf '              spawn line that caused classification is named above\n'
  return 1
}

# ── main check ────────────────────────────────────────────────────────────────
run_check() { # <explicit|default> <file...>
  local mode="$1"; shift
  local -a files=("$@")
  local f="" cls="" rel=""
  local total=0 spawning=0 compliant=0 rejected=0 notspawn=0
  local -a rejected_files=() bad_paths=()
  local explicit=0
  [ "$mode" = explicit ] && explicit=1

  if [ "$explicit" -eq 1 ]; then
    local -a keep=()
    for f in "${files[@]}"; do
      if [ ! -e "$f" ]; then
        _err "no such file: $f"
        bad_paths+=("$f")
        continue
      fi
      if ! _is_shell_script "$f"; then
        _err "not a shell script (no .sh, no shell shebang, not a *run-demo* runner): $f"
        bad_paths+=("$f")
        continue
      fi
      keep+=("$f")
    done
    if [ "${#bad_paths[@]}" -gt 0 ]; then
      _err "${#bad_paths[@]} path(s) in the explicit list were rejected — nothing was checked; refusing to print a green over a list it could not read."
      return 1
    fi
    files=("${keep[@]}")
    _info "scope=explicit list (${#files[@]} script(s))"
  else
    local collected=""
    collected="$(_collect_default_scope)" || return 2
    files=()
    while IFS= read -r rel; do
      [ -n "$rel" ] || continue
      files+=("$rel")
    done <<<"$collected"
    if [ "${#files[@]}" -eq 0 ]; then
      _err "the tracked tree yielded no shell script — an empty scope is never a PASS"
      _err "  (looking under $SCOPE_ROOT); pass an explicit file list if that is wrong."
      return 2
    fi
    _info "scope=tracked tree ($SCOPE_ROOT) — ${#files[@]} shell script(s)"
    # The tracked paths are RELATIVE to the checkout, so analyze them from there: a
    # run from any other cwd must check the same files (without this, every path
    # resolved against the caller's cwd, so a run from elsewhere failed every file
    # and reported them all rejected — correct fail-closed behaviour, wrong verdict).
    cd "$SCOPE_ROOT" || {
      _err "cannot change to $SCOPE_ROOT — the tracked paths are relative to it (exit 2)"
      return 2
    }
  fi

  _info "analyzer=python3 $(python3 -c 'import platform;print(platform.python_version())' 2>/dev/null)"

  for f in "${files[@]}"; do
    total=$((total + 1))
    # classification first — a fact about the TEXT, taken from the analysis. It is
    # deliberately not derived from the verdict below, so forcing the verdict to
    # success (the selftest's neuter proof) cannot also erase the classification.
    _classify "$f"
    cls="$_CHECK_CLASS"
    if [ "$cls" = spawn ]; then spawning=$((spawning + 1)); fi
    # NEUTER-MARK[verdict]: the rejection verdict call — the neuter proof forces
    # THIS call to success and requires the same fixture to be accepted.
    if ! _check_script "$f"; then
      rejected=$((rejected + 1))
      rejected_files+=("$f")
      continue
    fi
    if [ "$cls" = spawn ]; then
      compliant=$((compliant + 1))
    else
      notspawn=$((notspawn + 1))
    fi
  done

  printf '\n'
  printf '%s: %d script(s) in scope — %d server-spawning (%d compliant, %d rejected), %d not server-spawning\n' \
    "$PROG" "$total" "$spawning" "$compliant" "$rejected" "$notspawn"

  if [ "$rejected" -gt 0 ]; then
    printf '%s: REJECTED — %s\n' "$PROG" "${rejected_files[*]}" >&2
    printf '%s: a script that spawns a crier server, or every server-spawning script in an explicit list, must meet the trap+ownership contract (see the header of this file, CR-GAP-069)\n' "$PROG" >&2
    return 1
  fi
  if [ "$explicit" -eq 1 ] && [ "$spawning" -eq 0 ]; then
    _err "no script in the explicit list ($total file(s)) spawns a crier server — nothing was enforced; refusing a vacuous PASS"
    return 1
  fi
  printf '%s: PASS — every server-spawning script meets the cleanup contract\n' "$PROG"
  return 0
}

main() {
  if [ "$#" -gt 0 ]; then
    case "$1" in
      --help | -h)
        usage
        return 0
        ;;
      --selftest)
        if [ ! -r "$SELFTEST_SCRIPT" ]; then
          _err "$SELFTEST_SCRIPT is missing — nothing to prove (exit 2)"
          return 2
        fi
        bash "$SELFTEST_SCRIPT"
        return $?
        ;;
      --*) _err "unknown option: $1"; usage >&2; return 2 ;;
    esac
    command -v python3 >/dev/null 2>&1 || {
      _err "'python3' is not on PATH — the text analyzer cannot run, so nothing"
      _err "  would be verified. Install it and re-run (never a silent skip)."
      return 2
    }
    run_check explicit "$@"
    return $?
  fi

  command -v python3 >/dev/null 2>&1 || {
    _err "'python3' is not on PATH — the text analyzer cannot run, so nothing"
    _err "  would be verified. Install it and re-run (never a silent skip)."
    return 2
  }
  run_check default
  return $?
}

main "$@"
