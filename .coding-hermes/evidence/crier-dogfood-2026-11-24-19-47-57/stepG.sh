#!/bin/bash
# Step G: restart the server against the durable backend, then verify unacked survival.
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
echo "=== stop (documented 'make stop' path) ==="
./bin/crier -stop -pidfile $H/crier.pid 2>&1
sleep 1
curl -s -o /dev/null -w 'health_after_stop=%{http_code}\n' http://localhost:18876/health
echo "=== cold restart #2 ==="
t0=$(date +%s.%N)
# CR-GAP-069 trap+ownership contract: the spawned server must not outlive this
# capture script, and the port it binds must be held by the pid we started.
trap '[ -n "$SRVPID" ] && kill "$SRVPID" 2>/dev/null' EXIT
CRIER_PORT=18876 CR_DATABASE_URL='postgres://crier:crier@localhost:15437/crier?sslmode=disable' ./bin/crier -pidfile $H/crier.pid > $H/server2.log 2>&1 &
SRVPID=$!
for i in $(seq 1 100); do
  code=$(/usr/bin/curl -s -o /dev/null -w '%{http_code}' http://localhost:18876/health)
  [ "$code" = "200" ] && break; sleep 0.1
done
. ./scripts/lib/port-guard.sh
assert_port_owned 18876 "$SRVPID" "dogfood evidence server (stepG)"
t1=$(date +%s.%N)
echo "HEALTH_200_RESTART_AFTER=$(echo "$t1 $t0" | awk '{printf "%.3f", $1-$2}') s"
echo "=== verify survival ==="
python3 /tmp/dogfood-crier/durable_queue.py verify
echo "VERIFY_RC=$?"
