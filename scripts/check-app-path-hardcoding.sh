#!/usr/bin/env sh
set -eu

REPO_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$REPO_ROOT"

pattern='["`][^"`\n]*(?:(?:\.lumina|\.Lumina)(?:[/\\]|["`])|backend\.json|\bCONFIG(?:[/\\]|["`]))'
matches="$(rg -n -P "$pattern" \
  --glob '*.go' \
  --glob '*.ts' \
  --glob '!apppaths/**' \
  --glob '!config/config.go' \
  --glob '!**/*_test.go' \
  --glob '!frontend/src/paths.ts' || true)"

if [ -n "$matches" ]; then
  printf '%s\n' 'AppRoot path literals remain outside centralized path or compatibility code:' >&2
  printf '%s\n' "$matches" >&2
  exit 1
fi
