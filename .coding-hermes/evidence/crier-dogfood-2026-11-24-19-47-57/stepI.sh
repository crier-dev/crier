#!/bin/bash
# Step I: clean durability round — produce2, wait lease expiry, stop/start, verify2.
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
python3 $H/durability_clean.py produce2; PR=$?
echo "PRODUCE2_RC=$PR"
echo "--- waiting 32s for the 3 unacked leases to expire (documented default 30s) ---"
sleep 32
./bin/crier -stop -pidfile $H/crier.pid 2>&1
echo "--- restart ---"
# CR-GAP-069 trap+ownership contract: the spawned server must not outlive this
# capture script, and the port it binds must be held by the pid we started.
trap '[ -n "$SRVPID" ] && kill "$SRVPID" 2>/dev/null' EXIT
CRIER_PORT=18876 CR_DATABASE_URL='postgres://crier:crier@localhost:15437/crier?sslmode=disable' ./bin/crier -pidfile $H/crier.pid > $H/server3.log 2>&1 &
SRVPID=$!
for i in $(seq 1 100); do
  code=$(/usr/bin/curl -s -o /dev/null -w '%{http_code}' http://localhost:18876/health)
  [ "$code" = "200" ] && break; sleep 0.1
done
. ./scripts/lib/port-guard.sh
assert_port_owned 18876 "$SRVPID" "dogfood evidence server (stepI)"
echo "server healthy"
sleep 2
python3 $H/durability_clean.py verify2; VR=$?
echo "VERIFY2_RC=$VR"
