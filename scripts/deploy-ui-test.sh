#!/usr/bin/env bash
# Sanctioned UAT promotion path (DF-CRIER-464). Never builds an image.
# Usage: scripts/deploy-ui-test.sh --image TAG --commit HEX --admin-token-file FILE
# Optional --container NAME selects the existing UAT container explicitly;
# otherwise discover the unique crier-ui-live alias on crier-ui-test-net.
# Requires bash, docker, jq, flock. The token file contains ONE raw token line.
# Preserve current env/mount/network/aliases/ports/restart policy and command;
# explicitly enforce sqlite /data/sessions.db, permissions on and guard off.
# Containers (not background host servers) own the server lifecycle. On failed
# read-back the unverified replacement is removed, never its persistent volume.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR
# shellcheck source=deploy-ui-test-lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/deploy-ui-test-lib.sh"

image='' expected_commit='' container='' token_file=''
reported_commit=unknown dirty=unknown
fail() { printf 'FAILED: %s\n' "$*" >&2; provenance_values >&2; exit 1; }
refuse() { printf 'REFUSED: %s; nothing promoted\n' "$*" >&2; provenance_values >&2; exit 1; }
while (($#)); do
  case "$1" in
    --image|--commit|--container|--admin-token-file)
      (($# >= 2)) && [[ -n "$2" ]] || refuse "missing value for $1"
      case "$1" in
        --image) image="$2" ;; --commit) expected_commit="$2" ;;
        --container) container="$2" ;; --admin-token-file) token_file="$2" ;;
      esac
      shift 2 ;;
    -h|--help)
      printf '%s\n' 'Usage: deploy-ui-test.sh --image TAG --commit HEX --admin-token-file FILE [--container NAME]' \
        'Checks binary provenance before removal and three live /version reads after start.'
      exit 0 ;;
    *) refuse "unknown argument: $1" ;;
  esac
done
[[ "$expected_commit" =~ ^[0-9a-f]{7,40}$ ]] || refuse 'commit must be 7-40 lowercase hexadecimal characters'
[[ -n "$image" && "$image" != -* ]] || refuse 'an image tag is required'
for dep in docker jq flock; do command -v "$dep" >/dev/null || refuse "missing dependency: $dep"; done

# Serialize sanctioned deploys. The private directory also protects the lock
# and temporary env-file (which must never be printed or passed as token argv).
umask 077
state_dir="${XDG_RUNTIME_DIR:-${HOME:?}/.cache}/crier-ui-test-deploy"
mkdir -p -- "$state_dir"
exec 9>"$state_dir/lock"
flock -n 9 || refuse 'another UAT deployment is running'
scratch="$(mktemp -d "$state_dir/run.XXXXXX")"
new_id='' verified=false
cleanup() {
  if [[ -n "$new_id" && "$verified" != true ]]; then
    docker rm -f "$new_id" >/dev/null 2>&1 || printf 'WARNING: could not remove unverified replacement %s\n' "$new_id" >&2
  fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Resolve once, then probe AND promote the immutable ID: a moved tag cannot
# substitute a different image between this gate and docker run.
image_id="$(docker image inspect --format '{{.Id}}' "$image")" || refuse 'image identity unreadable'
[[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || refuse 'invalid image identity'
line="$(docker run --rm --network none "$image_id" -version)" || refuse 'binary provenance unreadable'
parse_version_line "$line" || refuse 'binary provenance malformed or unknown'
provenance_matches || refuse 'candidate provenance does not match'
provenance_values

[[ -r "$token_file" && -s "$token_file" ]] || refuse 'a readable nonempty --admin-token-file is required'
token="$(<"$token_file")"
[[ -n "$token" && "$token" != *$'\n'* && "$token" != *$'\r'* ]] || refuse 'admin token must be one nonempty line'
if [[ -z "$container" ]]; then
  ids="$(docker ps -aq --filter network=crier-ui-test-net)" || refuse 'cannot discover existing UAT container'
  [[ -n "$ids" ]] || refuse 'no existing UAT container found; initial provisioning is not promotion'
  # Docker IDs from ps, not operator shell text.
  readarray -t candidates <<<"$ids"
  docker inspect "${candidates[@]}" >"$scratch/containers.json" || refuse 'cannot inspect UAT candidates'
  container="$(jq -er '[.[] | select(.NetworkSettings.Networks["crier-ui-test-net"].Aliases // [] | index("crier-ui-live"))] |
    if length == 1 then .[0].Id else error("UAT alias must identify exactly one container") end' "$scratch/containers.json")" || refuse 'ambiguous or missing UAT alias'
fi
docker inspect "$container" >"$scratch/old.json" || refuse 'existing container unreadable'
# Scope-check even an explicit name, before any destructive operation. Support
# exactly the current single-network volume/bind deployment; refuse unsupported
# layouts instead of silently dropping their settings.
jq -e 'length == 1 and (.[0] |
  (.NetworkSettings.Networks | keys == ["crier-ui-test-net"]) and
  (.NetworkSettings.Networks["crier-ui-test-net"].Aliases | index("crier-ui-live") != null) and
  (.Config.Entrypoint == ["crier"]) and
  (.Mounts | any(.Destination == "/data" and .RW == true)) and
  (.Mounts | all((.Type == "volume" or .Type == "bind") and
    ((.Destination | contains(",")) | not) and
    (((if .Type == "volume" then .Name else .Source end) | contains(",")) | not))) and
  (.Config.Env | all((contains("\n") or contains("\r")) | not)))' \
  "$scratch/old.json" >/dev/null || refuse 'unsupported or non-UAT container configuration'
old_id="$(jq -r '.[0].Id' "$scratch/old.json")"
name="$(jq -r '.[0].Name | ltrimstr("/")' "$scratch/old.json")"
# Preserve unrelated settings, replacing only the mandated UAT posture. Remove
# the obsolete CR_SESSION_PATH too; config.Load reads CR_SESSION_SQLITE_PATH.
jq -r '.[0].Config.Env[] | select(test("^(CR_SESSION_BACKEND|CR_SESSION_SQLITE_PATH|CR_SESSION_PATH|CR_PERMISSIONS_ENABLED|CR_PERMISSIONS_ADMIN_TOKEN|CR_GUARD_ENABLED)=") | not)' \
  "$scratch/old.json" >"$scratch/env"
printf '%s\n' 'CR_SESSION_BACKEND=sqlite' 'CR_SESSION_SQLITE_PATH=/data/sessions.db' \
  'CR_PERMISSIONS_ENABLED=true' 'CR_GUARD_ENABLED=false' \
  "CR_PERMISSIONS_ADMIN_TOKEN=$token" >>"$scratch/env"
unset token
port="$(jq -r '[.[0].Config.Env[] | select(startswith("CRIER_PORT=")) | split("=")[1]] | last // "8767"' "$scratch/old.json")"
[[ "$port" =~ ^[0-9]{1,5}$ ]] || refuse 'invalid container listener port'
((10#$port > 0 && 10#$port <= 65535)) || refuse 'invalid container listener port'
args=(run -d --name "$name" --network crier-ui-test-net --env-file "$scratch/env")
while IFS= read -r alias; do args+=(--network-alias "$alias"); done < <(jq -r '.[0].NetworkSettings.Networks["crier-ui-test-net"].Aliases[]' "$scratch/old.json")
while IFS= read -r mount; do args+=(--mount "$mount"); done < <(jq -r '.[0].Mounts[] |
  "type=" + .Type + ",src=" + (if .Type == "volume" then .Name else .Source end) + ",dst=" + .Destination +
  (if .RW then "" else ",readonly" end)' "$scratch/old.json")
while IFS= read -r binding; do args+=(-p "$binding"); done < <(jq -r '.[0].HostConfig.PortBindings // {} | to_entries[] |
  .key as $port | .value[] | (if .HostIp == "" then "" elif (.HostIp | contains(":")) then "[" + .HostIp + "]:" else .HostIp + ":" end) + .HostPort + ":" + $port' "$scratch/old.json")
restart="$(jq -r '.[0].HostConfig.RestartPolicy | .Name + (if .Name == "on-failure" and .MaximumRetryCount > 0 then ":" + (.MaximumRetryCount | tostring) else "" end)' "$scratch/old.json")"
[[ -z "$restart" ]] || args+=(--restart "$restart")
for field in User WorkingDir; do
  value="$(jq -r --arg f "$field" '.[0].Config[$f] // ""' "$scratch/old.json")"
  if [[ -n "$value" ]]; then
    case "$field" in User) args+=(--user "$value") ;; WorkingDir) args+=(--workdir "$value") ;; esac
  fi
done
args+=(--entrypoint=crier "$image_id")
while IFS= read -r -d '' arg; do args+=("$arg"); done < <(jq -j '.[0].Config.Cmd // [] | .[] | . + "\u0000"' "$scratch/old.json")

# Remove FIRST to release the alias and sqlite writer before the replacement.
docker rm -f "$old_id" >/dev/null || fail 'old container removal failed; replacement not started'
new_id="$(docker "${args[@]}")" || fail 'replacement start failed (old container already removed)'
[[ "$new_id" =~ ^[0-9a-f]{64}$ ]] || { new_id=''; fail 'replacement returned no valid container ID'; }
assert_owner() {
  local owner
  owner="$(docker inspect --format '{{.Id}} {{.Image}} {{.State.Running}}' "$name")" || return 1
  [[ "$owner" == "$new_id $image_id true" ]]
}
ready=false
for ((attempt=1; attempt<=30; attempt++)); do
  assert_owner || fail 'replacement no longer owns the UAT container name/image'
  if docker exec "$new_id" wget -q -T 2 -O /dev/null "http://127.0.0.1:$port/health"; then ready=true; break; fi
  sleep 1
done
[[ "$ready" == true ]] || fail 'replacement listener did not become ready'
# Probe INSIDE the exact replacement, not an edge proxy or another listener.
# Any bad reading fails immediately; later clean evidence cannot erase it.
for ((attempt=1; attempt<=3; attempt++)); do
  reported_commit=unknown dirty=unknown
  assert_owner || fail 'replacement ownership changed during verification'
  body="$(docker exec "$new_id" wget -q -T 2 -O - "http://127.0.0.1:$port/version")" || fail "live provenance unreadable (read $attempt/3)"
  parse_version_json "$body" || fail "live provenance malformed (read $attempt/3)"
  provenance_matches || fail "live provenance disagrees (read $attempt/3)"
  printf 'verified /version read %s/3: ' "$attempt"; provenance_values
  [[ "$attempt" == 3 ]] || sleep 1
done
verified=true
printf 'PROMOTED: %s as %s (%s); three live provenance reads agree\n' "$image" "$name" "$image_id"
