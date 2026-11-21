#!/bin/sh
# Starts gc-mcp with settings from ~/.config/gc-mcp/env (0600). Loopback only.
set -eu
ENV_FILE="${GC_MCP_ENV_FILE:-$HOME/.config/gc-mcp/env}"
[ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE (see docs/runbook.md section 2)" >&2; exit 2; }
set -a; . "$ENV_FILE"; set +a
DIR="$(cd "$(dirname "$0")/.." && pwd)"
exec nice -n 10 node "$DIR/dist/main.js"
