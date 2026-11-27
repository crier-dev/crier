#!/usr/bin/env bash
set -u
OUT=${1:?output directory}
ROOT=/home/kara/crier
PORT=28971
BIN="$OUT/crier"
mkdir -p "$OUT"
cd "$ROOT"
HEAD=$(git rev-parse HEAD)
printf 'HEAD=%s\n' "$HEAD" > "$OUT/metadata.txt"
printf 'cwd=%s\n' "$PWD" >> "$OUT/metadata.txt"
cat /proc/loadavg > "$OUT/load-before.txt"
{
  echo '== BUILD =='
  /usr/bin/time -p env GOTOOLCHAIN=auto go build -ldflags "-X github.com/crier-dev/crier/internal/buildinfo.Version=dogfood-$HEAD -X github.com/crier-dev/crier/internal/buildinfo.Commit=$HEAD -X github.com/crier-dev/crier/internal/buildinfo.BuildTime=2026-11-27T20:50:20Z" -o "$BIN" ./cmd/server
} > "$OUT/build.stdout" 2> "$OUT/build.stderr"
printf 'build_rc=%s\n' "$?" >> "$OUT/metadata.txt"
ls -l "$BIN" >> "$OUT/metadata.txt"
"$BIN" -version > "$OUT/version.txt" 2>&1 || true
umask 077
"$BIN" keygen -out "$OUT/alice1.key" -id dogfood-alice-1 > "$OUT/keygen-alice1.txt" 2>&1
"$BIN" keygen -out "$OUT/alice2.key" -id dogfood-alice-2 > "$OUT/keygen-alice2.txt" 2>&1
"$BIN" keygen -out "$OUT/alice-ts.key" -id dogfood-alice-ts > "$OUT/keygen-alice-ts.txt" 2>&1
"$BIN" keygen -out "$OUT/alice.key" -id dogfood-alice > "$OUT/keygen-alice.txt" 2>&1
"$BIN" keygen -out "$OUT/bob.key" -id dogfood-bob > "$OUT/keygen-bob.txt" 2>&1
SERVER_PID=''
cleanup() {
  if test -n "$SERVER_PID" && kill -0 "$SERVER_PID" 2>/dev/null; then
    env -u CR_DATABASE_URL -u DATABASE_URL -u CRIER_DATABASE_URL CR_REQUIRE_AGENT_SIG=true CR_GUARD_ENABLED=false "$BIN" -stop -pidfile "$OUT/crier.pid" > "$OUT/stop.txt" 2>&1 || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT HUP INT TERM
: > "$OUT/server.stdout"
: > "$OUT/server.stderr"
(env -u CR_DATABASE_URL -u DATABASE_URL -u CRIER_DATABASE_URL CR_REQUIRE_AGENT_SIG=true CR_GUARD_ENABLED=false CRIER_PORT=$PORT "$BIN" -pidfile "$OUT/crier.pid" > "$OUT/server.stdout" 2> "$OUT/server.stderr") &
SERVER_PID=$!
printf 'server_pid=%s\n' "$SERVER_PID" >> "$OUT/metadata.txt"
start_ns=$(date +%s%N)
health_rc=1
for i in $(seq 1 60); do
  code=$(curl -sS -o "$OUT/health-first.json" -w '%{http_code}' "http://127.0.0.1:$PORT/health" 2> "$OUT/health-first.err") || code=000
  if test "$code" = 200; then health_rc=0; break; fi
  sleep 0.2
done
end_ns=$(date +%s%N)
printf 'health_rc=%s\n' "$health_rc" >> "$OUT/metadata.txt"
printf 'cold_health_ms=%s\n' "$(( (end_ns - start_ns) / 1000000 ))" >> "$OUT/metadata.txt"
cat /proc/loadavg > "$OUT/load-after-boot.txt"
if test "$health_rc" -ne 0; then exit 11; fi
run_client() {
  label=$1; shift
  cat /proc/loadavg > "$OUT/load-$label-before.txt"
  set +e
  /usr/bin/time -p -o "$OUT/time-$label.txt" python3 "$@" > "$OUT/$label.stdout" 2> "$OUT/$label.stderr"
  rc=$?
  set -e
  printf '%s_rc=%s\n' "$label" "$rc" >> "$OUT/metadata.txt"
  cat /proc/loadavg > "$OUT/load-$label-after.txt"
}
run_client roundtrip-cold clients/python/round_trip.py --server "http://127.0.0.1:$PORT" --id dogfood-alice-1 --key "$OUT/alice1.key" --topic dogfood-roundtrip-cold
run_client roundtrip-warm clients/python/round_trip.py --server "http://127.0.0.1:$PORT" --id dogfood-alice-2 --key "$OUT/alice2.key" --topic dogfood-roundtrip-warm
run_client two-agents clients/python/two_agents.py --server "http://127.0.0.1:$PORT" --alice-id dogfood-alice --alice-key "$OUT/alice.key" --bob-id dogfood-bob --bob-key "$OUT/bob.key"
if node --version > "$OUT/node-version.txt" 2>&1; then
  run_client typescript node clients/typescript/round-trip.ts --server "http://127.0.0.1:$PORT" --id dogfood-alice-ts --key "$OUT/alice-ts.key" --topic dogfood-roundtrip-typescript
else
  printf 'node unavailable\n' > "$OUT/typescript.stdout"
  printf 'node unavailable\n' > "$OUT/typescript.stderr"
  printf 'typescript_rc=127\n' >> "$OUT/metadata.txt"
fi
curl -sS -o "$OUT/version-live.json" -w '%{http_code}\n' "http://127.0.0.1:$PORT/version" > "$OUT/version-live.status" 2> "$OUT/version-live.err" || true
curl -sS -o "$OUT/status-live.json" -w '%{http_code}\n' "http://127.0.0.1:$PORT/status" > "$OUT/status-live.status" 2> "$OUT/status-live.err" || true
cat /proc/loadavg > "$OUT/load-before-stop.txt"
exit 0
