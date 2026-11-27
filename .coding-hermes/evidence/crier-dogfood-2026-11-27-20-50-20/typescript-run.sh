#!/usr/bin/env bash
set -u
OUT=/home/kara/.hermes/cache/scratch/crier-dogfood-2026-11-27-20-50-20/typescript
ROOT=/home/kara/crier
PORT=28972
mkdir -p "$OUT"
cd "$ROOT"
HEAD=$(git rev-parse HEAD)
env -u CR_DATABASE_URL -u DATABASE_URL -u CRIER_DATABASE_URL CR_REQUIRE_AGENT_SIG=true CR_GUARD_ENABLED=false go build -ldflags "-X github.com/crier-dev/crier/internal/buildinfo.Version=dogfood-ts-$HEAD -X github.com/crier-dev/crier/internal/buildinfo.Commit=$HEAD -X github.com/crier-dev/crier/internal/buildinfo.BuildTime=2026-11-27T20:50:20Z" -o "$OUT/crier" ./cmd/server > "$OUT/build.out" 2> "$OUT/build.err"
umask 077
"$OUT/crier" keygen -out "$OUT/alice.key" -id dogfood-ts > "$OUT/keygen.out" 2> "$OUT/keygen.err"
SERVER_PID=''
cleanup() {
  if test -n "$SERVER_PID" && kill -0 "$SERVER_PID" 2>/dev/null; then
    env -u CR_DATABASE_URL -u DATABASE_URL -u CRIER_DATABASE_URL CR_REQUIRE_AGENT_SIG=true CR_GUARD_ENABLED=false "$OUT/crier" -stop -pidfile "$OUT/crier.pid" > "$OUT/stop.out" 2>&1 || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT HUP INT TERM
(env -u CR_DATABASE_URL -u DATABASE_URL -u CRIER_DATABASE_URL CR_REQUIRE_AGENT_SIG=true CR_GUARD_ENABLED=false CRIER_PORT=$PORT "$OUT/crier" -pidfile "$OUT/crier.pid" > "$OUT/server.out" 2> "$OUT/server.err") &
SERVER_PID=$!
for i in $(seq 1 60); do code=$(curl -sS -o "$OUT/health.json" -w '%{http_code}' "http://127.0.0.1:$PORT/health" 2> "$OUT/health.err") || code=000; test "$code" = 200 && break; sleep .2; done
cat /proc/loadavg > "$OUT/load-before.txt"
/usr/bin/time -p -o "$OUT/time.txt" node clients/typescript/round-trip.ts --server "http://127.0.0.1:$PORT" --id dogfood-ts --key "$OUT/alice.key" --topic dogfood-ts-topic > "$OUT/roundtrip.stdout" 2> "$OUT/roundtrip.stderr"
rc=$?
cat /proc/loadavg > "$OUT/load-after.txt"
printf 'rc=%s\nhead=%s\n' "$rc" "$HEAD" > "$OUT/metadata.txt"
exit "$rc"
