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

# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a

if [ "$LANE" = "fallback" ]; then
  : "${FIREWORKS_KEY_GITREINS:?FIREWORKS_KEY_GITREINS missing in $ENV_FILE}"
  if [ "${#FIREWORKS_KEY_GITREINS}" -lt 30 ]; then
    echo "FATAL: FIREWORKS_KEY_GITREINS looks truncated (${#FIREWORKS_KEY_GITREINS} chars); a short key wedges the judge as a silent hang, refusing" >&2
    exit 3
  fi
  export GITREINS_LLM_BASE_URL="https://api.fireworks.ai/inference/v1"
  export GITREINS_LLM_MODEL="accounts/fireworks/models/kimi-k3"
  export GITREINS_LLM_API_KEY="$FIREWORKS_KEY_GITREINS"
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
