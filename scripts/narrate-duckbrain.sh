#!/usr/bin/env bash
# narrate-duckbrain.sh — cwd-safe DuckBrain narration wrapper (QA-CRIER-22).
#
# The duckbrain CLI resolves its namespaces/ store RELATIVE TO THE CALLER'S CWD.
# Any remember run from a project workdir silently creates a SECOND, nested
# store (<repo>/namespaces/) and the narration never reaches ~/duckbrain —
# measured twice on crier (2026-09-18 QA lane; tick 373 foreman narration).
# This wrapper pins cwd to the canonical store root before every CLI call, so
# a stray store cannot be created regardless of the caller's cwd. It also
# refuses to run without the authoritative embedding environment, which
# prevents the silent 384-dim local-model fallback.
#
# Usage: narrate-duckbrain.sh <duckbrain.js args...>
# Env:   DUCKBRAIN_EMBEDDING_* must be exported first (source the
#        duckbrain-http.service override); DUCKBRAIN_STORE_ROOT overrides the
#        store root (default ~/duckbrain).
set -euo pipefail

STORE="${DUCKBRAIN_STORE_ROOT:-$HOME/duckbrain}"
if [ ! -d "$STORE" ]; then
  echo "narrate-duckbrain: store root not found: $STORE" >&2
  exit 2
fi
if [ -z "${DUCKBRAIN_EMBEDDING_API_KEY:-}" ]; then
  echo "narrate-duckbrain: DUCKBRAIN_EMBEDDING_* env not set — load the" >&2
  echo "duckbrain-http.service override (Environment= lines) before calling;" >&2
  echo "without it the CLI auto-provider falls back to a local 384-dim model" >&2
  echo "and the row lands with a vector recall cannot match." >&2
  exit 2
fi
cd "$STORE"
exec node bin/duckbrain.js "$@"
