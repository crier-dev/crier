#!/bin/bash
# Step E: documented round_trip + two_agents runs on the fresh client surface.
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
S=http://localhost:18876
echo "=== curl GET /agents (who is pre-registered?) ==="
/usr/bin/curl -s -o $H/agents.json -w 'HTTP=%{http_code}\n' "$S/agents" ; cat $H/agents.json
echo
echo "=== round_trip.py (documented transcript) ==="
/usr/bin/time -o $H/rt.time -f "RT_WALL=%e s" python3 clients/python/round_trip.py --server $S --id alice --key $H/alice.key > $H/rt.out 2> $H/rt.err
echo "rc=$?"; cat $H/rt.time; tail -30 $H/rt.out; echo "--- stderr:"; tail -5 $H/rt.err
