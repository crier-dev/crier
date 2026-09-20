#!/usr/bin/env bash
#
# scripts/lib/require-docker-compose.sh — compose-plugin preflight + one-shot
# self-heal for the bunker battery (DF-CRIER-187, QA-CRIER-6).
#
# Sourced (bash 4+), safe under `set -euo pipefail`:
#
#     . "$REPO_ROOT/scripts/lib/require-docker-compose.sh"
#     require_docker_compose
#
# WHY THIS EXISTS
# ---------------
# The bunker agent bootstrap installs only the docker CLI + rootless extra.
# On a fresh JIT agent the `docker compose` v2 plugin is ABSENT, so
# `docker compose up -d --build` is parsed as `docker compose` (a nonexistent
# image) with `-d` as a flag — rc=125 "unknown shorthand flag: 'd' in -d".
# Hit live on bunker-las-02/67708cff (2026-09-15 and 2026-09-19 batteries),
# killing both the docker-deploy and chaos-shutdown cells. This helper fails
# EARLY with rc=3 and a diagnostic naming the missing plugin, after one
# best-effort self-heal install.
#
# `docker --version` proves only the CLI; only `docker compose version`
# proves the plugin.

# Internal: one best-effort install attempt of the compose v2 binary plugin.
# Pinned upstream URL; idempotent (skip if the file already exists and a
# plugin is detected by a later check).
_dcr_self_heal_install() {
    local dest="${DCR_CLI_PLUGINS_DIR:-$HOME/.docker/cli-plugins}"
    local pin="v2.32.1"
    local url="${DCR_COMPOSE_URL:-https://github.com/docker/compose/releases/download/${pin}/docker-compose-linux-x86_64}"
    mkdir -p "$dest" 2>/dev/null || return 1
    [ -x "$dest/docker-compose" ] && return 0
    ( command -v curl >/dev/null 2>&1 ) || return 1
    curl -fsSL --max-time 240 -o "$dest/docker-compose" "$url" || return 1
    chmod +x "$dest/docker-compose" 2>/dev/null || return 1
    return 0
}

# Main preflight. ASCII mirrors of typography (AC text uses em dashes):
# prints the FATAL line exactly when failed, exits 3.
require_docker_compose() {
    if docker compose version >/dev/null 2>&1; then
        docker compose version
        return 0
    fi
    # Self-heal rail: one best-effort attempt, silent on success.
    if _dcr_self_heal_install; then
        if docker compose version >/dev/null 2>&1; then
            docker compose version
            return 0
        fi
        # Install may have worked but context PATH may also need a shim.
        if [ -x "$HOME/bin/docker-compose" ]; then
            export PATH="$HOME/bin:$PATH"
            if docker compose version >/dev/null 2>&1; then
                docker compose version
                return 0
            fi
        fi
    fi
    echo 'bunker battery: FATAL: docker compose plugin missing — install via "mkdir -p ~/.docker/cli-plugins && curl -fsSL https://github.com/docker/compose/releases/download/v2.32.1/docker-compose-linux-x86_64 -o ~/.docker/cli-plugins/docker-compose && chmod +x ~/.docker/cli-plugins/docker-compose" or file as QA environmental gap' >&2
    return 3
}
