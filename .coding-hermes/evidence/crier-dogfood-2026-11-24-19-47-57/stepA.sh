#!/bin/bash
# Dogfood fire crier-dogfood-2026-11-24-19-47-57 — Step A: documented build path, timed.
cd /home/kara/crier || exit 9
echo "=== make build (cold-ish tree) ==="
/usr/bin/time -o /tmp/dogfood-crier/build.time -f "BUILD_WALL=%e s  BUILD_MAXRSS=%M KB" make build 2>&1 | tail -5
cat /tmp/dogfood-crier/build.time
ls -la bin/crier
echo "=== identity ==="
./bin/crier -version 2>&1
