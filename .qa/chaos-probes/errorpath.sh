#!/usr/bin/env bash
# QA-CRIER-39 — real chaos-errorpath premise for crier.
# The generic bunker-qa probe runs `timeout 20 $BIN` with no config, which for
# crier prints MCP/in-process startup INFO and exits 0 — the "missing config"
# premise is never exercised. This probe drives a GENUINELY broken config
# (CRIER_PORT that cannot parse) and asserts the documented failure:
#   rc != 0 AND the log names the failing variable.
# Exit contract (bunker-qa project-probe hook): 0=OK, 1=FAIL, 2=INFO env gap.
set -u
dir=$(mktemp -d) || exit 2
trap 'rm -rf "$dir"' EXIT
bin="$dir/crier"
if ! go build -o "$bin" ./cmd/server >"$dir/build.log" 2>&1; then
  echo "could not build cmd/server: $(head -2 "$dir/build.log")"
  exit 2
fi
env -i PATH="$PATH" HOME="$HOME" CRIER_PORT=definitely-not-a-port \
  "$bin" >"$dir/run.log" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then
  echo "crier exited rc=0 on an unparseable CRIER_PORT — error path NOT exercised"
  exit 1
fi
if grep -q 'invalid CRIER_PORT' "$dir/run.log"; then
  echo "clean documented failure on broken config: rc=$rc, log names the variable ($(head -1 "$dir/run.log" | cut -c1-160))"
  exit 0
fi
echo "rc=$rc but the log does not name the failing config variable: $(head -2 "$dir/run.log" | tr '\n' ' ' | cut -c1-200)"
exit 1
