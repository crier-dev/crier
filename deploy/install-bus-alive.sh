#!/usr/bin/env bash
# OPS-CRIER-002: install the crier bus-alive USER units (probe + timer).
#
# Copies deploy/crier-bus-alive.{service,timer} into ~/.config/systemd/user/,
# reloads the user manager, then enables + starts the timer. Idempotent:
# re-running refreshes the unit copies and re-asserts enablement, changing
# nothing that is already correct — safe to re-run after editing the units.
#
# The service's ExecStart runs scripts/bus-alive.sh from the CANONICAL checkout
# (the worktree that produced the units is temporary and gets reaped), so this
# must be run from, or the units must have been installed from, that checkout.
# Docs: docs/operations.md §7.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
PROBE="/home/kara/crier/scripts/bus-alive.sh"

# Fail closed before creating a timer that fires into a missing probe: a unit
# that cannot exec its script fails every 2 minutes and the alarm is noise.
if [ ! -x "$PROBE" ]; then
    echo "install-bus-alive: probe not executable at $PROBE" >&2
    echo "install-bus-alive: run this from the canonical crier checkout (deploy/install-bus-alive.sh)" >&2
    exit 1
fi

install -Dm 0644 "$SCRIPT_DIR/crier-bus-alive.service" "$UNIT_DIR/crier-bus-alive.service"
install -Dm 0644 "$SCRIPT_DIR/crier-bus-alive.timer"   "$UNIT_DIR/crier-bus-alive.timer"

systemctl --user daemon-reload
systemctl --user enable --now crier-bus-alive.timer

echo "installed: $UNIT_DIR/crier-bus-alive.service"
echo "installed: $UNIT_DIR/crier-bus-alive.timer"
systemctl --user list-timers crier-bus-alive.timer --no-pager
echo "alarm + heartbeat records: journalctl --user -t crier-bus-alive -n 50 --no-pager"
