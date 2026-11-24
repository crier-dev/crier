#!/bin/bash
# Clean-slate durability arm: fresh queue id, produce 10, ack 7, restart, verify 3 survive.
cd /home/kara/crier || exit 9
H=/tmp/dogfood-crier
./bin/crier keygen -out $H/queue2.key -id dog-queue2-1124 >/dev/null 2>&1
echo "queue2 key ready"
