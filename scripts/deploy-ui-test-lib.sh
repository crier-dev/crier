#!/usr/bin/env bash
# Pure provenance decisions; source without contacting Docker. jq is used only
# for the HTTP JSON format. Unknown evidence is never equivalent to clean.
parse_version_line() {
  reported_commit=unknown
  dirty=unknown
  local line="$1"
  if [[ "$line" =~ ^crier\ (v[^[:space:]]+|dev[^[:space:]]*)-([0-9a-f]{7,40})(-dirty)?$ ]]; then
    reported_commit="${BASH_REMATCH[2]}"
    dirty=false
    # A stamped git-describe version can carry -dirty BEFORE the commit too.
    [[ "$line" != *-dirty* ]] || dirty=true
    return 0
  fi
  return 1
}

parse_version_json() {
  reported_commit=unknown
  dirty=unknown
  local body="$1"
  jq -e 'type == "object" and (.commit | type == "string") and
    (.commit | test("^[0-9a-f]{7,40}$")) and
    (.modified | type == "boolean") and (.version | type == "string")' \
    <<<"$body" >/dev/null 2>&1 || return 1
  reported_commit="$(jq -r '.commit' <<<"$body")"
  dirty="$(jq -r '.modified or (.version | contains("-dirty"))' <<<"$body")"
}

provenance_matches() {
  local expected="${expected_commit:-}"
  [[ "$expected" =~ ^[0-9a-f]{7,40}$ ]] || return 1
  [[ "$reported_commit" == "$expected"* ]] || return 1
  [[ "$dirty" == false ]] || return 1 # DIRTY_GATE: selftest mutates this decision only
}

provenance_values() {
  printf 'expected commit=%s; reported commit=%s; dirty=%s\n' \
    "$expected_commit" "$reported_commit" "$dirty"
}
