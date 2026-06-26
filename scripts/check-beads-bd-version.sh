#!/usr/bin/env bash
set -euo pipefail

# check-beads-bd-version.sh — guard against the recurring gc<->bd version skew.
#
# gc's version_compat gate DISABLES the native bead store at runtime when gc's
# linked github.com/steveyegge/beads library version != the installed bd binary
# version (logged as `native_store_unavailable gate=version_compat`). That
# silently degrades convoy/reconcile and corrupts the assignee index (stranding
# refinery work). It recurs whenever an upstream merge resets the go.mod beads
# `require` version (e.g. v1.0.5 -> v1.0.4) and gc is rebuilt with no guard.
#
# This check fails the build when the EFFECTIVE beads module version in go.mod
# (respecting replace directives) does not match the bd binary on PATH, so the
# skew is caught at build time instead of after hours of silent store
# degradation. When bd is not installed (e.g. CI/docker image builds) it skips
# rather than fails, so it only ever blocks a genuinely mismatched build.
#
# See gas-city-infra memory: dolt-connection-drops-bd-version-mismatch.

GOMOD="${1:-go.mod}"

# Effective beads version (honours replace directives), resolved offline.
beads_ver="$(GOFLAGS=-mod=mod GOPROXY=off go list -m -f '{{.Version}}' \
  github.com/steveyegge/beads 2>/dev/null || true)"
if [ -z "$beads_ver" ]; then
  # Fallback: parse the require line directly from go.mod.
  beads_ver="$(awk '$1=="github.com/steveyegge/beads"{print $2; exit}' "$GOMOD" 2>/dev/null || true)"
fi
beads_ver="${beads_ver#v}"

if [ -z "$beads_ver" ]; then
  echo "check-beads-bd-version: could not determine go.mod beads version" >&2
  exit 1
fi

if ! command -v bd >/dev/null 2>&1; then
  echo "check-beads-bd-version: bd not on PATH — skipping version match (go.mod beads=v${beads_ver})"
  exit 0
fi

bd_ver="$(bd version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
if [ -z "$bd_ver" ]; then
  echo "check-beads-bd-version: could not parse 'bd version' output — skipping" >&2
  exit 0
fi

if [ "$beads_ver" != "$bd_ver" ]; then
  cat >&2 <<EOF
check-beads-bd-version: BEADS VERSION SKEW — refusing to build.

  go.mod linked beads = v${beads_ver}
  installed bd binary = v${bd_ver}

gc's version_compat gate will DISABLE the native bead store at runtime
(native_store_unavailable), silently degrading convoy/reconcile and corrupting
the assignee index (stranding refinery work).

Fix: align them — bump the go.mod 'github.com/steveyegge/beads' require to
v${bd_ver} (or install bd v${beads_ver}) so the linked library matches the
binary, then rebuild. This skew typically returns after an upstream merge
resets the go.mod pin; do not ship a mismatched build.
EOF
  exit 1
fi

echo "check-beads-bd-version: OK (go.mod beads=v${beads_ver} matches bd=v${bd_ver})"
