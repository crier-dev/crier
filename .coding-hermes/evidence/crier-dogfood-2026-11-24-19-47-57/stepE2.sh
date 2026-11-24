#!/bin/bash
# Step E2: recovery arms for the 409 lockout, then the documented journey under new ids.
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
S=http://localhost:18876
KEY=$H/alice.key
ID=alice
TS=$(date +%s)
PATHP=/agents/$ID
# python staged signer (stdlib) — write nothing persistent except output
python3 $H/sign_delete.py "$ID" "$TS" > $H/sig.txt 2>$H/sig.err; echo "sign rc=$?"; cat $H/sig.err
SIG=$(cat $H/sig.txt)
echo "=== DELETE /agents/alice signed with the NEW (wrong) key ==="
/usr/bin/curl -s -o $H/del.json -w 'HTTP=%{http_code}\n' -X DELETE "$S$PATHP" \
  -H "X-Agent-ID: $ID" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG"; cat $H/del.json; echo
echo "=== does an admin/operator path exist? ==="
grep -rn 'CR_MGMT\|management' README.md | head -4
rm -f $H/alice.key $H/bob.key
./bin/crier keygen -out $H/alice2.key -id alice2 >/dev/null 2>&1
./bin/crier keygen -out $H/bob2.key -id bob2 >/dev/null 2>&1
echo "=== round_trip.py under a fresh id (documented path, cont'd) ==="
/usr/bin/time -o $H/rt.time -f "RT_WALL=%e s" python3 clients/python/round_trip.py --server $S --id alice2 --key $H/alice2.key > $H/rt.out 2>&1
echo "rc=$?"; cat $H/rt.time; grep -c 'OK   ' $H/rt.out; grep 'FAIL\|ERROR' $H/rt.out | head -5; tail -12 $H/rt.out
