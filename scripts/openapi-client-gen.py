#!/usr/bin/env python3
"""openapi-client-gen.py — derive a working HTTP client from an OpenAPI document.

INT-MUSTER-005: this is the spec-driven half of the battery's client-generation
precedence. It reads the OpenAPI document, finds the operations the E2E flow needs
BY operationId (`registryRegisterAgent`, `inboxDeliver`, `inboxRetrieve`,
`inboxAck`), derives each one's method, path template, request-body keys and
signature-header names FROM THE DOCUMENT, and emits:

  <out>/client.json   the derivation, with the spec's sha256 as provenance
  <out>/client.sh     a client that performs the whole flow using only values
                      the generator derived from the spec

Nothing about the endpoints is written by hand: a spec that renames a path, a
body key or a signature header changes the emitted client (the battery's cell
proves that with a mutation, and asserts that every path the client targets is
a path key of the spec document).

FAIL CLOSED: a spec that cannot be read, cannot be parsed, is missing any of the
four operations, or no longer declares the contract this flow needs (a path
placeholder other than `{id}`, a deliver body with no payload key, an ack body
with no lease/message-ids key, a retrieve op with no signature header params)
exits 2 with the reason on stderr — the caller turns that into a FAIL, never a
silent skip.

The YAML reader below is a BOUNDED subset reader, deliberately so: block
mappings, block sequences, plain/quoted scalars, block scalars (`|`, `>` and
their chomping variants), comments, and flow collections (kept as raw text —
the document's flow sequences are `required: [a, b]` and `enum: [...]` lists,
which the derivation splits itself). Tabs in indentation, a line that cannot be
placed, and a duplicate key inside one mapping are errors, not guesses. It is
not a general YAML implementation and does not aim to be one.

Usage:
  openapi-client-gen.py --spec docs/openapi.yaml --out DIR [--base-url URL]
  openapi-client-gen.py --spec docs/openapi.yaml --print-endpoints
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys

EXIT_SPEC = 2  # fail-closed exit code: the spec could not be turned into a client

HTTP_METHODS = ("get", "post", "put", "patch", "delete", "head", "options", "trace")

# operationId -> the role it plays in the flow. The IDS are the spec's contract
# with this client; the endpoints themselves are read out of the document.
OPERATIONS = (
    ("register", "registryRegisterAgent"),
    ("deliver", "inboxDeliver"),
    ("retrieve", "inboxRetrieve"),
    ("ack", "inboxAck"),
)

# The register call carries the agent's identity and, on a server that enforces
# per-agent signatures (CR_REQUIRE_AGENT_SIG=true, the battery's posture), its
# ed25519 public key. Both names are asserted to be DECLARED by the spec's
# register request body — the client never invents a wire key.
REGISTER_BODY_KEYS = ("id", "public_key")


class SpecError(Exception):
    """A spec problem the caller must treat as a FAIL (never a skip)."""


# ── bounded YAML subset reader ────────────────────────────────────────────────


def _strip_comment(line: str) -> str:
    """Drop a trailing `#` comment, honouring single/double quotes."""
    out = []
    quote = ""
    prev = ""
    for ch in line:
        if quote:
            out.append(ch)
            if ch == quote:
                quote = ""
        elif ch in "\"'":
            quote = ch
            out.append(ch)
        elif ch == "#" and (prev == "" or prev.isspace()):
            break
        else:
            out.append(ch)
        prev = ch
    return "".join(out)


def _logical_lines(text: str) -> list:
    """(indent, content) for every content-bearing line; comments/blanks dropped."""
    lines = []
    for n, raw in enumerate(text.splitlines(), 1):
        lead = raw[: len(raw) - len(raw.lstrip())]
        if "\t" in lead:
            raise SpecError("line %d: tab indentation is not part of the subset" % n)
        line = _strip_comment(raw).rstrip()
        if not line.strip():
            continue
        lines.append((len(line) - len(line.lstrip(" ")), line.strip()))
    return lines


def _unquote(text: str) -> str:
    if len(text) >= 2 and text[0] == text[-1] and text[0] in "\"'":
        return text[1:-1]
    return text


def _split_kv(content: str):
    """Split `key: value` at the first colon outside quotes followed by EOL/space.

    Returns None when the line is not a mapping entry (a plain scalar such as a
    URL, or a sequence item that is not a mapping).
    """
    quote = ""
    for i, ch in enumerate(content):
        if quote:
            if ch == quote:
                quote = ""
            continue
        if ch in "\"'":
            quote = ch
            continue
        if ch == ":" and (i + 1 == len(content) or content[i + 1].isspace()):
            key = _unquote(content[:i].strip())
            if not key:
                return None
            return key, content[i + 1:].strip()
    return None


_BLOCK_MARKERS = "|>"


def _scalar(text: str):
    if text == "":
        return None
    if _is_flow(text):
        return text  # flow collection, kept raw for the derivation to split
    return _unquote(text)


def _is_flow(text: str) -> bool:
    return len(text) >= 2 and text[0] in "[{" and text[-1] in "]}"


def _entry_value(lines, i, indent, val):
    """Value of an entry whose key sits at `indent`; returns (value, next_index)."""
    if val[:1] and val[0] in _BLOCK_MARKERS:  # block scalar: consume the indented body
        body = []
        while i < len(lines) and lines[i][0] > indent:
            body.append(lines[i][1])
            i += 1
        return "\n".join(body), i
    if val != "":
        return _scalar(val), i
    if i < len(lines) and lines[i][0] > indent:
        child = lines[i][0]
        if lines[i][1].startswith("- "):
            return _parse_sequence(lines, i, child)
        return _parse_mapping(lines, i, child)
    return None, i


def _parse_mapping(lines, i, indent):
    out = {}
    while i < len(lines):
        ind, content = lines[i]
        if ind < indent or content.startswith("- "):
            break
        if ind > indent:
            raise SpecError("unplaceable line %r at indent %d (expected %d)" % (content, ind, indent))
        kv = _split_kv(content)
        if kv is None:
            raise SpecError("unplaceable line %r at indent %d" % (content, ind))
        key, val = kv
        if key in out:
            raise SpecError("duplicate key %r in one mapping at indent %d" % (key, indent))
        i += 1
        out[key], i = _entry_value(lines, i, indent, val)
    return out, i


def _parse_sequence(lines, i, indent):
    items = []
    while i < len(lines) and lines[i][0] == indent and lines[i][1].startswith("- "):
        content = lines[i][1][2:].strip()
        i += 1
        if content == "":
            if i < len(lines) and lines[i][0] > indent:
                child = lines[i][0]
                if lines[i][1].startswith("- "):
                    sub, i = _parse_sequence(lines, i, child)
                else:
                    sub, i = _parse_mapping(lines, i, child)
                items.append(sub)
            else:
                items.append(None)
            continue
        kv = _split_kv(content)
        if kv is None:
            items.append(_scalar(content))
            continue
        # "- key: value" opens a mapping whose sibling keys sit at indent + 2
        item = {}
        key, val = kv
        item[key], i = _entry_value(lines, i, indent + 2, val)
        while i < len(lines) and lines[i][0] == indent + 2:
            nxt = _split_kv(lines[i][1])
            if nxt is None:
                break
            key, val = nxt
            if key in item:
                raise SpecError("duplicate key %r in one sequence item" % key)
            i += 1
            item[key], i = _entry_value(lines, i, indent + 2, val)
        items.append(item)
    return items, i


def parse_spec(text: str) -> dict:
    """Parse the document with the bounded subset reader (raises SpecError)."""
    lines = _logical_lines(text)
    if not lines:
        raise SpecError("document is empty")
    if lines[0][0] != 0:
        raise SpecError("document does not start at indent 0")
    doc, i = _parse_mapping(lines, 0, 0)
    if i != len(lines):
        raise SpecError("unparsed trailing lines: %r" % (lines[i][1],))
    return doc


# ── derivation ────────────────────────────────────────────────────────────────


def _flow_list(raw) -> list:
    """Split a flow sequence kept as raw text: `[a, b]` -> ['a', 'b']."""
    if raw is None:
        return []
    if isinstance(raw, list):
        return [str(x) for x in raw]
    text = str(raw).strip()
    if not text.startswith("[") or not text.endswith("]"):
        return []
    inner = text[1:-1].strip()
    if not inner:
        return []
    return [_unquote(part.strip()) for part in inner.split(",") if part.strip()]


def _operations(doc: dict) -> dict:
    """operationId -> (path, method, operation) over the whole paths block."""
    paths = doc.get("paths")
    if not isinstance(paths, dict) or not paths:
        raise SpecError("document has no non-empty `paths` block")
    found = {}
    for path, item in paths.items():
        if not isinstance(item, dict):
            raise SpecError("path item %r is not a mapping" % path)
        for method in HTTP_METHODS:
            op = item.get(method)
            if not isinstance(op, dict):
                continue
            opid = op.get("operationId")
            if not isinstance(opid, str) or not opid:
                raise SpecError("%s %s has no operationId" % (method.upper(), path))
            if opid in found:
                raise SpecError("operationId %r appears more than once" % opid)
            found[opid] = (path, method, op)
    return found


def _json_schema(operation: dict):
    """The operation's application/json request schema, or None."""
    body = operation.get("requestBody")
    if not isinstance(body, dict):
        return None
    content = body.get("content")
    if not isinstance(content, dict):
        return None
    media = content.get("application/json")
    if not isinstance(media, dict):
        return None
    schema = media.get("schema")
    if not isinstance(schema, dict):
        return None
    return schema


def _header_params(operation: dict, doc: dict) -> list:
    """Header parameter names declared by an operation, following `$ref`s."""
    params = operation.get("parameters")
    if not isinstance(params, list):
        return []
    components = doc.get("components")
    shared = components.get("parameters", {}) if isinstance(components, dict) else {}
    names = []
    for param in params:
        if not isinstance(param, dict):
            continue
        ref = param.get("$ref")
        if isinstance(ref, str):
            key = ref.rsplit("/", 1)[-1]
            target = shared.get(key)
            if not isinstance(target, dict):
                raise SpecError("unresolvable parameter $ref %r" % ref)
            param = target
        if param.get("in") != "header":
            continue
        name = param.get("name")
        if not isinstance(name, str) or not name:
            raise SpecError("header parameter without a name")
        names.append(name)
    return names


def _base_url(doc: dict, override: str) -> str:
    if override:
        return override
    servers = doc.get("servers")
    if isinstance(servers, list):
        for entry in servers:
            if isinstance(entry, dict) and isinstance(entry.get("url"), str):
                return entry["url"]
    raise SpecError("no base URL: pass --base-url or declare servers[0].url in the spec")


def derive(doc: dict, override_base_url: str) -> dict:
    """Turn a parsed spec into the client contract (raises SpecError)."""
    by_id = _operations(doc)
    plan = {"base_url": _base_url(doc, override_base_url), "operations": {}}

    for role, opid in OPERATIONS:
        if opid not in by_id:
            raise SpecError("the spec declares no operationId %r (needed for %s)" % (opid, role))
        path, method, op = by_id[opid]
        placeholders = [p for p in _placeholder_names(path) if p != "id"]
        if placeholders:
            raise SpecError(
                "%s (%s %s) has path parameters this client cannot fill: %s"
                % (role, method.upper(), path, ", ".join(placeholders))
            )
        entry = {"operationId": opid, "method": method.upper(), "path": path}

        schema = _json_schema(op)
        if role == "register":
            declared = set(_flow_list(_schema_value(schema, "required")))
            declared |= set(_schema_properties(schema).keys())
            missing = [k for k in REGISTER_BODY_KEYS if k not in declared]
            if missing:
                raise SpecError(
                    "%s (%s %s) request body does not declare %s"
                    % (role, method.upper(), path, ", ".join(missing))
                )
            entry["body_keys"] = list(REGISTER_BODY_KEYS)
        elif role == "deliver":
            if schema is None:
                raise SpecError("%s (%s %s) declares no application/json request body" % (role, method.upper(), path))
            required = _flow_list(_schema_value(schema, "required"))
            properties = list(_schema_properties(schema).keys())
            payload_key = required[0] if required else (properties[0] if properties else None)
            if not payload_key:
                raise SpecError("%s (%s %s) request body declares no payload key" % (role, method.upper(), path))
            entry["body_payload_key"] = payload_key
        elif role == "ack":
            if schema is None:
                raise SpecError("%s (%s %s) declares no application/json request body" % (role, method.upper(), path))
            keys = _flow_list(_schema_value(schema, "required")) or list(_schema_properties(schema).keys())
            lease = [k for k in keys if "lease" in k.lower()]
            ids = [k for k in keys if k not in lease and ("message" in k.lower() or "id" in k.lower())]
            if not lease or not ids:
                raise SpecError(
                    "%s (%s %s) request body does not declare a lease key and a message-ids key (found %s)"
                    % (role, method.upper(), path, ", ".join(keys) or "none")
                )
            entry["body_lease_key"] = lease[0]
            entry["body_ids_key"] = ids[0]

        if role in ("retrieve", "ack"):
            headers = _header_params(op, doc)
            if not headers:
                raise SpecError(
                    "%s (%s %s) declares no signature header parameters — the signed "
                    "retrieve/ack contract is not in the spec" % (role, method.upper(), path)
                )
            entry["sign_headers"] = headers

        plan["operations"][role] = entry

    retrieve_headers = plan["operations"]["retrieve"]["sign_headers"]
    ack_headers = plan["operations"]["ack"]["sign_headers"]
    if set(retrieve_headers) != set(ack_headers):
        raise SpecError(
            "retrieve and ack disagree on their signature headers (%s vs %s)"
            % (", ".join(retrieve_headers), ", ".join(ack_headers))
        )
    return plan


def _placeholder_names(path: str) -> list:
    names = []
    rest = path
    while "{" in rest and "}" in rest:
        start = rest.index("{")
        end = rest.index("}", start)
        names.append(rest[start + 1:end])
        rest = rest[end + 1:]
    return names


def _schema_value(schema, key):
    if isinstance(schema, dict):
        return schema.get(key)
    return None


def _schema_properties(schema) -> dict:
    props = _schema_value(schema, "properties")
    return props if isinstance(props, dict) else {}


# ── emission ──────────────────────────────────────────────────────────────────

CLIENT_TEMPLATE = r"""#!/usr/bin/env bash
# client.sh — GENERATED by scripts/openapi-client-gen.py from @@SPEC@@
# (sha256 @@SHA@@).
#
# Every endpoint, method, body key and signature-header name below was derived
# from that document at generation time; nothing here was written by hand. The
# signed requests follow the e2e-battery convention: ed25519 over
# "<METHOD>\n<path>\n<unix-seconds>", emitted as the spec's own header names.
#
# Usage: client.sh --out DIR --base-url URL --token T --agent ID --key KEYFILE \
#                  --payload-file FILE
# Writes one step-<role>.json per call into DIR and the raw retrieve body to
# DIR/retrieve-body.json.
set -uo pipefail

OUT=""; BASE_URL=""; TOKEN=""; AGENT=""; KEYFILE=""; PAYLOAD_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) OUT="$2"; shift 2 ;;
    --base-url) BASE_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --agent) AGENT="$2"; shift 2 ;;
    --key) KEYFILE="$2"; shift 2 ;;
    --payload-file) PAYLOAD_FILE="$2"; shift 2 ;;
    *) echo "client.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done
for v in OUT BASE_URL TOKEN AGENT KEYFILE PAYLOAD_FILE; do
  eval "val=\$$v"
  [ -n "$val" ] || { echo "client.sh: missing --$(echo "$v" | tr 'A-Z_' 'a-z-')" >&2; exit 2; }
done
mkdir -p "$OUT"

# ── endpoints (DERIVED FROM @@SPEC@@) ────────────────────────────────────────
OP_REGISTER_METHOD="@@REGISTER_METHOD@@";  OP_REGISTER_PATH="@@REGISTER_PATH@@"
OP_DELIVER_METHOD="@@DELIVER_METHOD@@";    OP_DELIVER_PATH="@@DELIVER_PATH@@"
OP_RETRIEVE_METHOD="@@RETRIEVE_METHOD@@";  OP_RETRIEVE_PATH="@@RETRIEVE_PATH@@"
OP_ACK_METHOD="@@ACK_METHOD@@";            OP_ACK_PATH="@@ACK_PATH@@"
REGISTER_BODY_KEYS="@@REGISTER_KEYS@@"
DELIVER_PAYLOAD_KEY="@@DELIVER_PAYLOAD_KEY@@"
ACK_LEASE_KEY="@@ACK_LEASE_KEY@@"
ACK_IDS_KEY="@@ACK_IDS_KEY@@"
SIGN_HEADERS="@@SIGN_HEADERS@@"

fill() { printf '%s' "$1" | sed "s/{id}/$AGENT/g"; }
step() { # role method path template http
  printf '{"step":"%s","via":"client.sh","method":"%s","path":"%s","template":"%s","http":%s}\n' \
    "$1" "$2" "$3" "$4" "$5" > "$OUT/step-$1.json"
}
sign() { # METHOD PATH -> SIG_TS / SIG_SIG (battery convention, spec header names)
  local ts; ts="$(date +%s)"
  printf '%s\n%s\n%s' "$1" "$2" "$ts" > "$OUT/sign-pl.txt"
  SIG_SIG="$(openssl pkeyutl -sign -rawin -inkey "$KEYFILE" -in "$OUT/sign-pl.txt" 2>/dev/null | xxd -p -c 128)"
  SIG_TS="$ts"
}
sig_args() { # METHOD PATH -> SIG_ARGS array, using the names the spec declares
  sign "$1" "$2"
  SIG_ARGS=()
  local h id_name ts_name sig_name
  id_name="$(printf '%s\n' $SIGN_HEADERS | sed -n 1p)"
  ts_name="$(printf '%s\n' $SIGN_HEADERS | sed -n 2p)"
  sig_name="$(printf '%s\n' $SIGN_HEADERS | sed -n 3p)"
  for h in "$id_name" "$ts_name" "$sig_name"; do
    case "$h" in
      *[Ii][Dd]*) SIG_ARGS+=(-H "$h: $AGENT") ;;
      *[Tt][Ss]*) SIG_ARGS+=(-H "$h: $SIG_TS") ;;
      *)          SIG_ARGS+=(-H "$h: $SIG_SIG") ;;
    esac
  done
}
call() { # METHOD URL [data] [extra...] -> HTTP_CODE / BODY_FILE written
  local method="$1" url="$2" data="${3:-}"; shift 3 2>/dev/null || shift $#
  local out
  if [ -n "$data" ]; then
    out="$(curl -s -w '\n%{http_code}' -X "$method" "$url" -H 'Content-Type: application/json' \
      -H "Authorization: Bearer $TOKEN" "$@" -d "$data" 2>>"$OUT/driver.log")"
  else
    out="$(curl -s -w '\n%{http_code}' -X "$method" "$url" -H "Authorization: Bearer $TOKEN" \
      "$@" 2>>"$OUT/driver.log")"
  fi
  HTTP_CODE="${out##*$'\n'}"; BODY="${out%$'\n'*}"
  case "$HTTP_CODE" in ''|*[!0-9]*) HTTP_CODE=0 ;; esac
}

# 1. register — body keys from the spec's register request schema
PUBHEX="$(openssl pkey -in "$KEYFILE" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)"
[ -n "$PUBHEX" ] || { echo "client.sh: could not derive the public key from $KEYFILE" >&2; exit 2; }
REG_PATH="$(fill "$OP_REGISTER_PATH")"
REG_BODY="{"
first=1
for k in $REGISTER_BODY_KEYS; do
  case "$k" in
    id) v="$AGENT" ;;
    public_key) v="$PUBHEX" ;;
    *) echo "client.sh: spec-derived register key without a value: $k" >&2; exit 2 ;;
  esac
  [ "$first" = 1 ] || REG_BODY="$REG_BODY,"
  REG_BODY="$REG_BODY\"$k\":\"$v\""; first=0
done
REG_BODY="$REG_BODY}"
call "$OP_REGISTER_METHOD" "$BASE_URL$REG_PATH" "$REG_BODY"
step register "$OP_REGISTER_METHOD" "$REG_PATH" "$OP_REGISTER_PATH" "$HTTP_CODE"
[ "$HTTP_CODE" = "201" ] || exit 1

# 2. deliver — the payload rides under whatever key the spec's deliver schema names
DEL_PATH="$(fill "$OP_DELIVER_PATH")"
DEL_BODY="{\"$DELIVER_PAYLOAD_KEY\":$(cat "$PAYLOAD_FILE")}"
call "$OP_DELIVER_METHOD" "$BASE_URL$DEL_PATH" "$DEL_BODY"
step deliver "$OP_DELIVER_METHOD" "$DEL_PATH" "$OP_DELIVER_PATH" "$HTTP_CODE"
[ "$HTTP_CODE" = "201" ] || exit 1

# 3. signed retrieve — sign "<METHOD>\n<path>\n<ts>" with the spec's header names
RET_PATH="$(fill "$OP_RETRIEVE_PATH")"
sig_args "$OP_RETRIEVE_METHOD" "$RET_PATH"
call "$OP_RETRIEVE_METHOD" "$BASE_URL$RET_PATH" "" "${SIG_ARGS[@]}"
step retrieve "$OP_RETRIEVE_METHOD" "$RET_PATH" "$OP_RETRIEVE_PATH" "$HTTP_CODE"
printf '%s' "$BODY" > "$OUT/retrieve-body.json"
[ "$HTTP_CODE" = "200" ] || exit 1

# 4. ack — lease key + message-ids key from the spec's ack schema
LEASE="$(python3 - "$OUT/retrieve-body.json" "$ACK_LEASE_KEY" <<'PYEOF'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit(0)
value = body.get(sys.argv[2], "")
print(value if isinstance(value, str) else "")
PYEOF
)"
MSG_IDS="$(python3 - "$OUT/retrieve-body.json" "$ACK_IDS_KEY" <<'PYEOF'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit(0)
msgs = body.get("messages") or []
ids = [m.get("id") for m in msgs if isinstance(m, dict) and m.get("id")]
print(",".join(ids))
PYEOF
)"
ACK_PATH="$(fill "$OP_ACK_PATH")"
ACK_BODY="{\"$ACK_LEASE_KEY\":\"$LEASE\",\"$ACK_IDS_KEY\":["
first=1
IFS=',' read -r -a _ids <<< "$MSG_IDS"
for m in "${_ids[@]:-}"; do
  [ -n "$m" ] || continue
  [ "$first" = 1 ] || ACK_BODY="$ACK_BODY,"
  ACK_BODY="$ACK_BODY\"$m\""; first=0
done
ACK_BODY="$ACK_BODY]}"
sig_args "$OP_ACK_METHOD" "$ACK_PATH"
call "$OP_ACK_METHOD" "$BASE_URL$ACK_PATH" "$ACK_BODY" "${SIG_ARGS[@]}"
step ack "$OP_ACK_METHOD" "$ACK_PATH" "$OP_ACK_PATH" "$HTTP_CODE"
[ "$HTTP_CODE" = "204" ] || exit 1
exit 0
"""


def emit_clients(plan: dict, spec_path: str, digest: str, out_dir: str) -> dict:
    os.makedirs(out_dir, exist_ok=True)
    ops = plan["operations"]
    contract = {
        "spec": spec_path,
        "spec_sha256": digest,
        "generated_by": "scripts/openapi-client-gen.py",
        "base_url": plan["base_url"],
        "operations": ops,
        "register_body_keys": ops["register"]["body_keys"],
    }
    with open(os.path.join(out_dir, "client.json"), "w", encoding="utf-8") as fh:
        json.dump(contract, fh, indent=2, sort_keys=True)
        fh.write("\n")

    text = CLIENT_TEMPLATE
    for placeholder, value in (
        ("@@SPEC@@", spec_path),
        ("@@SHA@@", digest),
        ("@@REGISTER_METHOD@@", ops["register"]["method"]),
        ("@@REGISTER_PATH@@", ops["register"]["path"]),
        ("@@DELIVER_METHOD@@", ops["deliver"]["method"]),
        ("@@DELIVER_PATH@@", ops["deliver"]["path"]),
        ("@@RETRIEVE_METHOD@@", ops["retrieve"]["method"]),
        ("@@RETRIEVE_PATH@@", ops["retrieve"]["path"]),
        ("@@ACK_METHOD@@", ops["ack"]["method"]),
        ("@@ACK_PATH@@", ops["ack"]["path"]),
        ("@@REGISTER_KEYS@@", " ".join(ops["register"]["body_keys"])),
        ("@@DELIVER_PAYLOAD_KEY@@", ops["deliver"]["body_payload_key"]),
        ("@@ACK_LEASE_KEY@@", ops["ack"]["body_lease_key"]),
        ("@@ACK_IDS_KEY@@", ops["ack"]["body_ids_key"]),
        ("@@SIGN_HEADERS@@", "\n".join(ops["retrieve"]["sign_headers"])),
    ):
        text = text.replace(placeholder, value)
    if "@@" in text:
        raise SpecError("client template still carries an unsubstituted placeholder")
    client_path = os.path.join(out_dir, "client.sh")
    with open(client_path, "w", encoding="utf-8") as fh:
        fh.write(text)
    os.chmod(client_path, 0o755)
    return contract


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--spec", required=True, help="OpenAPI document to derive from")
    parser.add_argument("--out", help="directory for the generated client")
    parser.add_argument("--base-url", default="", help="override the spec's servers[0].url")
    parser.add_argument("--print-endpoints", action="store_true", help="print the derivation as JSON")
    args = parser.parse_args(argv)

    try:
        with open(args.spec, "rb") as fh:
            raw = fh.read()
    except OSError as exc:
        print("openapi-client-gen: CANNOT READ SPEC %s: %s" % (args.spec, exc), file=sys.stderr)
        return EXIT_SPEC
    digest = hashlib.sha256(raw).hexdigest()
    try:
        doc = parse_spec(raw.decode("utf-8"))
        plan = derive(doc, args.base_url)
    except SpecError as exc:
        print("openapi-client-gen: SPEC REJECTED (%s): %s" % (args.spec, exc), file=sys.stderr)
        return EXIT_SPEC
    except UnicodeDecodeError as exc:
        print("openapi-client-gen: SPEC NOT UTF-8 (%s): %s" % (args.spec, exc), file=sys.stderr)
        return EXIT_SPEC

    ops = plan["operations"]
    print("spec: %s" % args.spec)
    print("spec sha256: %s" % digest)
    print("parsed: %d path(s)" % len(doc.get("paths", {})))
    for role, _ in OPERATIONS:
        entry = ops[role]
        extra = ""
        if "body_payload_key" in entry:
            extra = " payload=%s" % entry["body_payload_key"]
        elif "body_lease_key" in entry:
            extra = " lease=%s ids=%s" % (entry["body_lease_key"], entry["body_ids_key"])
        elif "sign_headers" in entry:
            extra = " sign=%s" % ",".join(entry["sign_headers"])
        print("derived %-8s %-4s %s (operationId %s)%s" % (role, entry["method"], entry["path"], entry["operationId"], extra))

    if args.print_endpoints:
        print(json.dumps(plan, indent=2, sort_keys=True))
        return 0
    if not args.out:
        print("openapi-client-gen: --out is required (or use --print-endpoints)", file=sys.stderr)
        return EXIT_SPEC
    emit_clients(plan, args.spec, digest, args.out)
    print("generated: %s/client.json + %s/client.sh" % (args.out, args.out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
