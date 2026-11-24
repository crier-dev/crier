#!/bin/bash
# Step B/C/D: keygen, postgres on an override port (5437 held by canopy-pg), server cold boot.
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
echo "=== crier keygen (documented onboarding, no openssl) ==="
/usr/bin/time -o $H/keygen.time -f "KEYGEN_WALL=%e s" ./bin/crier keygen -out $H/alice.key -id alice 2>&1
cat $H/keygen.time
/usr/bin/time -o $H/keygen2.time -f "KEYGEN_WALL=%e s" ./bin/crier keygen -out $H/bob.key -id bob 2>&1
ls -l $H/alice.key $H/bob.key
echo "=== docker compose postgres (port override: 5437 taken) ==="
CRIER_PG_HOST_PORT=15437 docker compose up -d postgres 2>&1 | tail -3
echo "=== wait for healthy ==="
for i in $(seq 1 20); do
  st=$(docker inspect --format '{{.State.Health.Status}}' crier-postgres-1 2>/dev/null)
  echo "health=$st"; [ "$st" = "healthy" ] && break; sleep 1
done
echo "=== server cold boot (durable backend) ==="
CRIER_PORT=18876 CR_DATABASE_URL='postgres://crier:crier@localhost:15437/crier?sslmode=disable' ./bin/crier -pidfile $H/crier.pid > $H/server.log 2>&1 &
SRVPID=$!
t0=$(date +%s.%N)
for i in $(seq 1 100); do
  code=$(curl -s -o $H/health.json -w '%{http_code}' http://localhost:18876/health 2>/dev/null)
  [ "$code" = "200" ] && break
  sleep 0.1
done
t1=$(date +%s.%N)
echo "HEALTH_200_AFTER=$(echo "$t1 $t0" | awk '{printf "%.3f", $1-$2}') s (poll resolution 0.1s)"
echo "health body: $(cat $H/health.json)"
curl -s http://localhost:18876/status > $H/status.json && cat $H/status.json
echo; echo "srv pid: $SRVPID"; tail -5 $H/server.log
