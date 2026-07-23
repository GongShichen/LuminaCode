#!/usr/bin/env sh
set -eu

APP_ROOT="${1:?usage: install-app-layout.sh <app-root> <backend-bin> [installed-version]}"
BACKEND_BIN="${2:?usage: install-app-layout.sh <app-root> <backend-bin> [installed-version]}"
INSTALLED_VERSION="${3:-dev}"
REPO_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
. "$REPO_ROOT/scripts/app-paths.sh"
APP_ROOT="$(lumina_resolve_app_root "$APP_ROOT")"
APP_NEW="$APP_ROOT/app.new"
APP_OLD="$APP_ROOT/app.old"
NPM_BIN="${NPM:-npm}"
SWAPPED=0

case "$APP_ROOT" in
  /*) ;;
  *) printf 'error: AppRoot must be absolute: %s\n' "$APP_ROOT" >&2; exit 1 ;;
esac
if [ "$APP_ROOT" = "/" ]; then
  printf 'error: refusing unsafe AppRoot: %s\n' "$APP_ROOT" >&2
  exit 1
fi
if [ ! -x "$BACKEND_BIN" ]; then
  printf 'error: backend binary is not executable: %s\n' "$BACKEND_BIN" >&2
  exit 1
fi
if [ -e "$APP_NEW" ] || [ -e "$APP_OLD" ]; then
  printf 'error: stale installer staging directory exists under %s; inspect app.new/app.old before retrying\n' "$APP_ROOT" >&2
  exit 1
fi

rollback() {
  status=$?
  trap - 0
  if [ "$status" -ne 0 ] && [ "$SWAPPED" = "1" ]; then
    rm -rf "$APP_ROOT/app"
    if [ -d "$APP_OLD" ]; then
      mv "$APP_OLD" "$APP_ROOT/app"
    fi
  fi
  if [ -d "$APP_NEW" ]; then
    rm -rf "$APP_NEW"
  fi
  exit "$status"
}
trap rollback 0

mkdir -p "$APP_ROOT"
chmod 0700 "$APP_ROOT" 2>/dev/null || true
mkdir -p \
  "$APP_NEW/frontend" \
  "$APP_NEW/resources/defaults" \
  "$APP_NEW/resources/system" \
  "$APP_NEW/resources/skills" \
  "$APP_NEW/resources/teams" \
  "$APP_NEW/extensions" \
  "$APP_NEW/scripts"

cp -R "$REPO_ROOT/.Lumina/SYSTEM/." "$APP_NEW/resources/system/"
cp -R "$REPO_ROOT/.Lumina/SKILLS/." "$APP_NEW/resources/skills/"
cp -R "$REPO_ROOT/.Lumina/TEAM/." "$APP_NEW/resources/teams/"
cp "$REPO_ROOT/.Lumina/CONFIG/defaults.json.example" "$APP_NEW/resources/defaults/settings.example.json"
cp -R "$REPO_ROOT/frontend/dist" "$APP_NEW/frontend/"
cp "$REPO_ROOT/frontend/package.json" "$REPO_ROOT/frontend/package-lock.json" "$APP_NEW/frontend/"
cp "$REPO_ROOT/setup-searxng.sh" "$APP_NEW/scripts/setup-searxng.sh"
cp "$REPO_ROOT/scripts/app-paths.sh" "$APP_NEW/scripts/app-paths.sh"
cp "$REPO_ROOT/scripts/setup-arxiv-mcp.sh" "$APP_NEW/scripts/setup-arxiv-mcp.sh"
cp "$REPO_ROOT/scripts/setup-memory-models.sh" "$APP_NEW/scripts/setup-memory-models.sh"
cp "$REPO_ROOT/scripts/install-preflight.sh" "$APP_NEW/scripts/install-preflight.sh"
cp "$REPO_ROOT/scripts/install.sh" "$APP_NEW/scripts/install.sh"
cp "$REPO_ROOT/scripts/memory-models.lock" "$APP_NEW/scripts/memory-models.lock"

(CDPATH= cd -- "$APP_NEW/frontend" && "$NPM_BIN" ci --omit=dev)
rm -f "$APP_NEW/frontend/package-lock.json"

find "$APP_NEW" -type d -exec chmod 0755 '{}' ';'
find "$APP_NEW" -type f -exec chmod 0644 '{}' ';'
chmod 0755 "$APP_NEW/scripts/app-paths.sh" "$APP_NEW/scripts/setup-searxng.sh" "$APP_NEW/scripts/setup-arxiv-mcp.sh" "$APP_NEW/scripts/setup-memory-models.sh" "$APP_NEW/scripts/install-preflight.sh" "$APP_NEW/scripts/install.sh"

LUMINA_APP_ROOT="$APP_ROOT" "$BACKEND_BIN" shutdown >/dev/null 2>&1 || true
LUMINA_APP_ROOT="$APP_ROOT" "$BACKEND_BIN" layout migrate \
  --apply \
  --source "$APP_ROOT" \
  --project-root "$REPO_ROOT" \
  --packaged-resources "$APP_NEW/resources" \
  --installed-version "$INSTALLED_VERSION"

if [ -d "$APP_ROOT/app/extensions" ]; then
  cp -R "$APP_ROOT/app/extensions/." "$APP_NEW/extensions/"
fi

if [ -d "$APP_ROOT/app" ]; then
  mv "$APP_ROOT/app" "$APP_OLD"
fi
mv "$APP_NEW" "$APP_ROOT/app"
SWAPPED=1

test -f "$APP_ROOT/app/frontend/dist/index.js"
test -f "$APP_ROOT/app/resources/system/system-prompt.md"
LUMINA_APP_ROOT="$APP_ROOT" "$BACKEND_BIN" layout doctor --json >/dev/null

if [ -d "$APP_OLD" ]; then
  rm -rf "$APP_OLD"
fi
SWAPPED=0
trap - 0
