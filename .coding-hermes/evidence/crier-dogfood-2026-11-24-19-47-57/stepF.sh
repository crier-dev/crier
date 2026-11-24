#!/bin/bash
# Step F: two_agents journey on a shared bus (fresh ids to dodge the stale volume).
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
S=http://localhost:18876
/usr/bin/time -o $H/ta.time -f "TWO_AGENTS_WALL=%e s" python3 clients/python/two_agents.py --server $S \
  --alice-id dog-alice-1124 --alice-key $H/alice2.key --bob-id dog-bob-1124 --bob-key $H/bob2.key > $H/ta.out 2>&1
echo "rc=$?"; cat $H/ta.time; tail -25 $H/ta.out
