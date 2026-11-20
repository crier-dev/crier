#!/usr/bin/env bash
# gitreins-judge-lane.sh — INT-CI-042 durable judge failover for this repo.
#
# Primary lane (deepseek-v4-flash @ api.deepseek.com) is DEAD as of 2026-11-20:
# every tier-2 call 402'd "Payment Required" (QA-CRIER-59 run e0f88d05,
# QA-CRIER-066-R2 run e304a6a7, RELEASE-026 run a8911b7d, DF-CRIER-509 verdict
# 6f1ef4cf), and the ambient GITREINS_LLM_API_KEY now 401s as invalid/rotated
# (measured 2026-11-20, probe transcript in docs/judge-failover.md).
#
# This wrapper arms the verified fallback lane — fireworks kimi-k3 — as ONE
# unit (base URL + model + key), resolving the key internally from
# ~/.hermes/.env so it NEVER enters argv and is never echoed.
#
# Why a wrapper and not config: .gitreins/config.yaml's `defaults:` block is
# decoration — the tier-2 evaluator resolves its lane purely from the three
# GITREINS_LLM_* env vars. The ambient env unit is additionally MISWIRED
# (GITREINS_LLM_BASE_URL=fireworks but GITREINS_LLM_API_KEY=<dead deepseek
# key>), so a bare `gitreins` call 401s. The wrapper exports the consistent
# unit itself.
#
# Usage:
#   scripts/gitreins-judge-lane.sh <gitreins args...>         # fallback lane (default)
#   scripts/gitreins-judge-lane.sh --lane primary <args...>   # retry primary once re-funded
# Examples:
#   scripts/gitreins-judge-lane.sh task complete DF-CRIER-505
#   scripts/gitreins-judge-lane.sh judge DF-CRIER-505 --async
#   scripts/gitreins-judge-lane.sh task complete <id> --skip-tier2   # tier1-only close
#
# Verdicts land under .gitreins/history/<date>/<run-id>/verdict.json — locate
# by grepping the TASK ID, never by printed verdict id (they can diverge).
set -euo pipefail

ENV_FILE="${GITREINS_JUDGE_ENV_FILE:-$HOME/.hermes/.env}"
LANE=fallback
if [ "${1:-}" = "--lane" ]; then
  LANE="$2"; shift 2
fi

# Source the env file WITHOUT executing it. A plain `. "$ENV_FILE"` dies under
# set -e at the first line whose value carries shell-active characters (the
# shared Hermes .env has e.g. `API_SERVER_MODEL_NAME=Hermes Agent`, rc=127) —
# and the abort silently drops every key defined AFTER that line
# (FIREWORKS_KEY_GITREINS lives at the tail). Parse assignments instead.
while IFS= read -r envline || [ -n "$envline" ]; do
  envline=${envline%$'\r'}
  case "$envline" in ''|'#'*) continue ;; esac
  envkey=${envline%%=*}
  if [ "$envkey" = "$envline" ]; then
    continue  # no '=' -> not an assignment; never execute it
  fi
  export "$envkey=${envline#*=}"
done < "$ENV_FILE"

if [ "$LANE" = "fallback" ]; then
  # Length guard BEFORE any key export. Evidence (2026-11-20 live probe):
  # a 25-char fw_ key authenticates 200 against fireworks — the old 30-char
  # threshold falsified on a working key. The real hazard is the dot-truncated
  # storage shape (e.g. sk-c2c...3c4c), which lands well under 20 chars and
  # wedges the judge as a silent 401 hang. Guard at 20; if the primary var is
  # truncated or missing but a full-length key survives in GITREINS_LLM_API_KEY,
  # use it rather than refusing.
  lane_key="${FIREWORKS_KEY_GITREINS:-}"
  lane_src="FIREWORKS_KEY_GITREINS"
  if [ "${#lane_key}" -lt 20 ]; then
    fb_key="${GITREINS_LLM_API_KEY:-}"
    if [ "${#fb_key}" -ge 20 ]; then
      echo "WARN: FIREWORKS_KEY_GITREINS missing/truncated (${#lane_key} chars) in $ENV_FILE; using GITREINS_LLM_API_KEY" >&2
      lane_key="$fb_key"
      lane_src="GITREINS_LLM_API_KEY"
    else
      echo "FATAL: no usable fireworks key: FIREWORKS_KEY_GITREINS=${#lane_key} chars, GITREINS_LLM_API_KEY=${#fb_key} chars; a truncated key wedges the judge as a silent hang, refusing" >&2
      exit 3
    fi
  fi
  export GITREINS_LLM_BASE_URL="https://api.fireworks.ai/inference/v1"
  export GITREINS_LLM_MODEL="accounts/fireworks/models/kimi-k3"
  export GITREINS_LLM_API_KEY="$lane_key"
  echo "judge-lane: fallback lane armed (key source: $lane_src, ${#lane_key} chars)" >&2
elif [ "$LANE" = "primary" ]; then
  : "${DEEPSEEK_API_KEY:?DEEPSEEK_API_KEY missing in $ENV_FILE}"
  if [ "${#DEEPSEEK_API_KEY}" -lt 30 ]; then
    echo "FATAL: DEEPSEEK_API_KEY looks truncated (${#DEEPSEEK_API_KEY} chars); refusing" >&2
    exit 3
  fi
  export GITREINS_LLM_BASE_URL="https://api.deepseek.com/v1"
  export GITREINS_LLM_MODEL="deepseek-v4-flash"
  export GITREINS_LLM_API_KEY="$DEEPSEEK_API_KEY"
else
  echo "unknown lane: $LANE (use fallback|primary)" >&2
  exit 2
fi

exec ~/.local/bin/gitreins "$@"
