#!/usr/bin/env bash
# QA-CRIER-069: exercise the rendered snapshot and corruption cell in isolation.
set -euo pipefail
HARNESS="${BUNKER_QA_SH:-$HOME/.hermes/scripts/bunker-qa.sh}"
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT
bash "$HARNESS" __gen-remote sample > "$TMPD/rendered"
bash -n "$TMPD/rendered"
python3 - "$TMPD/rendered" "$TMPD/blocks" <<'PY'
import sys
s = open(sys.argv[1]).read()
snapshot = s[s.index('# QA-OFF-BY-ONE-15-R2: snapshot state candidates'):s.index('# qa_have_pip')]
start = s.index('if [ -x .qa/chaos-probes/corruption.sh ]')
wrapper = '( if cd "$QAREPO"; then\n'
if s[start-len(wrapper):start] == wrapper:
    start -= len(wrapper)
cell = s[start:s.index('# 9 · chaos-resource')]
open(sys.argv[2], 'w').write('snapshot() {\n' + snapshot + '\n}\nchaos() {\n' + cell + '\n}\n')
PY
source "$TMPD/blocks"
set +e
export HOME="$TMPD/home"
PROJ=sample
QAREPO="$HOME/qa-$PROJ"
LOGD="$HOME/qa-logs-test"
mkdir -p "$QAREPO/data" "$HOME/qa-sibling/data" "$LOGD" "$QAREPO/qa-logs-bogus"
printf '{"real":1}\n' > "$QAREPO/data/state.jsonl"
printf '{"foreign":1}\n' > "$HOME/qa-sibling/data/foreign.jsonl"
printf '{"harness":1}\n' > "$HOME/qa-evidence.jsonl"
printf '{"harness":1}\n' > "$QAREPO/qa-evidence.jsonl"
printf '{"log":1}\n' > "$QAREPO/qa-logs-bogus/log.jsonl"
printf '# state.jsonl qa-evidence.jsonl foreign.jsonl\n' > "$QAREPO/app.sh"
BIN=stub
qa_start_in_dir() { printf 'started\t0\n'; }
corr_grade() { printf '%s\t%s\n' "${GRADE:-OK}" 'control executed'; }
cell() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" > "$HOME/result"; }
cd "$HOME"
snapshot
chaos
result=$(< "$HOME/result")
if [[ "$result" != *'text/append-log state ./data/state.jsonl'* || "$result" != *$'\tOK\t'* ]]; then
  printf 'FAIL: expected real repo surface and OK controls; got %s\n' "$result"; exit 1
fi
if grep -E 'qa-evidence\.jsonl|qa-logs-|qa-sibling' "$LOGD/state-snapshot-text.txt"; then
  echo 'FAIL: harness or sibling state selected'; exit 1
fi
echo 'PASS: snapshot selects only repo state and both controls execute'
rm "$QAREPO/data/state.jsonl"
chaos
result=$(< "$HOME/result")
if [[ "$result" != *$'\tSKIP\t'* || "$result" != *'could not copy the surface'* ]]; then
  printf 'FAIL: missing surface must SKIP with reason; got %s\n' "$result"; exit 1
fi
echo 'PASS: vanished surface and zero controls grade SKIP with reason'
printf '{"real":1}\n' > "$QAREPO/data/state.jsonl"
GRADE=FAIL
chaos
result=$(< "$HOME/result")
if [[ "$result" != *$'\tFAIL\t'* ]]; then
  printf 'FAIL: executed failing control must remain FAIL; got %s\n' "$result"; exit 1
fi
echo 'PASS: executed failing control remains FAIL'
exit 0
