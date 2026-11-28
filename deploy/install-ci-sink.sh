#!/usr/bin/env bash
# OPS-CRIER-004: install the crier CI echo sink USER unit + its restart-loop bound.
#
# Before this row the unit existed only in ~/.config/systemd/user/ — untracked —
# which is how an unmanaged copy of the sink held :19012 for seven days while
# the supervised unit restart-looped (NRestarts 44745) with nobody noticing the
# posture was wrong. The unit is now version-controlled here and installed from
# this directory, so the restart policy has a reviewable home.
#
# Copies deploy/crier-ci-sink.service and the deploy/crier-ci-sink.service.d/
# drop-in into ~/.config/systemd/user/, then reloads the user manager.
# Idempotent: re-running refreshes the copies and changes nothing that is
# already correct.
#
# It deliberately does NOT restart the unit: restarts on this host are
# owner-gated, and the start-limit drop-in takes effect on the next natural
# restart. Use `systemctl --user restart crier-ci-sink.service` only with an
# owner decision in hand.
#
# The ExecStart target (/home/kara/bin/crier-ci-sink.py) still lives outside
# this repository — see .coding-hermes/evidence/OPS-CRIER-004.md for that
# residual.
#
# Docs: docs/operations.md §7.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SINK="/home/kara/bin/crier-ci-sink.py"

# Fail closed before installing a unit whose ExecStart cannot run: the unit would
# restart-loop on a missing script, which is the exact shape this row fixes.
if [ ! -r "$SINK" ]; then
    echo "install-ci-sink: sink script missing at $SINK" >&2
    exit 1
fi

install -Dm 0644 "$SCRIPT_DIR/crier-ci-sink.service" "$UNIT_DIR/crier-ci-sink.service"
install -Dm 0644 "$SCRIPT_DIR/crier-ci-sink.service.d/10-start-limit.conf" \
    "$UNIT_DIR/crier-ci-sink.service.d/10-start-limit.conf"

systemctl --user daemon-reload

echo "installed: $UNIT_DIR/crier-ci-sink.service"
echo "installed: $UNIT_DIR/crier-ci-sink.service.d/10-start-limit.conf"
systemctl --user show crier-ci-sink.service \
    -p ActiveState,SubState,MainPID,NRestarts,StartLimitIntervalUSec,StartLimitBurst
echo "port holder: $(ss -ltnp 'sport = :19012' | tail -n +2)"
