#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$SCRIPT_DIR/app-paths.sh"

while IFS='|' read -r name platform home_dir local_app_data override expected; do
  case "$name" in ''|'#'*) continue ;; esac
  case "$platform" in windows) continue ;; esac
  [ "$home_dir" = "-" ] && home_dir=""
  [ "$override" = "-" ] && override=""
  actual="$(lumina_resolve_app_root "$override" "$home_dir")"
  if [ "$actual" != "$expected" ]; then
    printf 'path contract %s: got %s, want %s\n' "$name" "$actual" "$expected" >&2
    exit 1
  fi
done < "$SCRIPT_DIR/../testdata/app-path-contract.tsv"

test "$(lumina_resolve_app_root '' '/tmp/lumina-home')" = "/tmp/lumina-home/.lumina"
test "$(lumina_resolve_app_root '/tmp/custom root' '/tmp/lumina-home')" = "/tmp/custom root"
test "$(lumina_resolve_app_root '~/custom' '/tmp/lumina-home')" = "/tmp/lumina-home/custom"
if lumina_resolve_app_root relative '/tmp/lumina-home' >/dev/null 2>&1; then exit 1; fi
if lumina_resolve_app_root / '/tmp/lumina-home' >/dev/null 2>&1; then exit 1; fi
