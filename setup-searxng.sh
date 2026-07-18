#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/app-paths.sh" ]; then
  . "$SCRIPT_DIR/app-paths.sh"
else
  . "$SCRIPT_DIR/scripts/app-paths.sh"
fi
APP_ROOT="$(lumina_resolve_app_root)"
SEARXNG_ROOT="${LUMINA_SEARXNG_ROOT:-${APP_ROOT}/state/services/searxng}"
SEARXNG_PORT="${LUMINA_SEARXNG_PORT:-8888}"
SEARXNG_BASE_URL="${LUMINA_WEB_SEARCH_BASE_URL:-http://127.0.0.1:${SEARXNG_PORT}}"
SEARXNG_IMAGE="${LUMINA_SEARXNG_IMAGE:-}"
COMPOSE_FILE="${SEARXNG_ROOT}/compose.yaml"
SETTINGS_FILE="${SEARXNG_ROOT}/settings.yml"
DEFAULTS_FILE="${APP_ROOT}/config/settings.json"
ACTION="${1:-install}"

log() {
  printf '%s\n' "$*"
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

compose_cmd() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    printf 'docker compose'
    return
  fi
  if command -v podman-compose >/dev/null 2>&1; then
    printf 'podman-compose'
    return
  fi
  if command -v podman >/dev/null 2>&1 && podman compose version >/dev/null 2>&1; then
    printf 'podman compose'
    return
  fi
  die "Docker Compose or Podman Compose is required."
}

random_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
    return
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 - <<'PY'
import secrets
print(secrets.token_hex(32))
PY
    return
  fi
  date +%s | shasum | awk '{print $1}'
}

write_settings() {
  mkdir -p "$SEARXNG_ROOT"
  chmod 0700 "$APP_ROOT/state" "$APP_ROOT/state/services" "$SEARXNG_ROOT" 2>/dev/null || true
  secret="$(random_secret)"
  cat > "$SETTINGS_FILE" <<EOF
use_default_settings: true

search:
  formats:
    - html
    - json

server:
  bind_address: "0.0.0.0"
  port: 8080
  secret_key: "${secret}"
  limiter: false
  image_proxy: false
EOF
  chmod 0600 "$SETTINGS_FILE" 2>/dev/null || true
}

write_compose() {
  mkdir -p "$SEARXNG_ROOT"
  image="${SEARXNG_IMAGE:-docker.io/searxng/searxng:latest}"
  cat > "$COMPOSE_FILE" <<EOF
services:
  searxng:
    image: ${image}
    container_name: lumina-searxng
    restart: unless-stopped
    ports:
      - "127.0.0.1:${SEARXNG_PORT}:8080"
    volumes:
      - "${SETTINGS_FILE}:/etc/searxng/settings.yml:ro"
    environment:
      - SEARXNG_BASE_URL=${SEARXNG_BASE_URL}/
EOF
  chmod 0600 "$COMPOSE_FILE" 2>/dev/null || true
}

select_image() {
  if [ -n "$SEARXNG_IMAGE" ]; then
    return
  fi
  cmd="$(compose_cmd)"
  images="docker.io/searxng/searxng:latest ghcr.io/searxng/searxng:latest"
  for image in $images; do
    attempts=0
    while [ "$attempts" -lt 2 ]; do
      attempts=$((attempts + 1))
      if command -v docker >/dev/null 2>&1 && printf '%s' "$cmd" | grep -q '^docker compose'; then
        if docker pull "$image" >/dev/null 2>&1; then
          SEARXNG_IMAGE="$image"
          export SEARXNG_IMAGE
          return
        fi
      elif command -v podman >/dev/null 2>&1; then
        if podman pull "$image" >/dev/null 2>&1; then
          SEARXNG_IMAGE="$image"
          export SEARXNG_IMAGE
          return
        fi
      else
        break
      fi
      sleep 2
    done
    warn "Could not pull $image; trying next image if available."
  done
  warn "Could not pre-pull a SearxNG image; compose will attempt its configured default."
}

merge_defaults() {
  mkdir -p "$(dirname "$DEFAULTS_FILE")"
  if command -v python3 >/dev/null 2>&1; then
    DEFAULTS_FILE="$DEFAULTS_FILE" SEARXNG_BASE_URL="$SEARXNG_BASE_URL" python3 - <<'PY'
import json, os
path = os.environ["DEFAULTS_FILE"]
base = os.environ["SEARXNG_BASE_URL"].rstrip("/")
try:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
except Exception:
    data = {}
defaults = {
    "web_search_enabled": True,
    "web_search_provider": "searxng",
    "web_search_base_url": base,
    "web_search_max_results": 10,
    "web_search_timeout_seconds": 20.0,
    "web_fetch_enabled": True,
    "web_fetch_require_search_result": True,
    "web_fetch_max_chars": 80000,
    "web_fetch_timeout_seconds": 20.0,
    "web_fetch_user_agent": "LuminaCode/1.0",
}
for key, value in defaults.items():
    data.setdefault(key, value)
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, ensure_ascii=False)
    f.write("\n")
os.chmod(path, 0o600)
PY
  else
    if [ ! -f "$DEFAULTS_FILE" ]; then
      cat > "$DEFAULTS_FILE" <<EOF
{
  "web_search_enabled": true,
  "web_search_provider": "searxng",
  "web_search_base_url": "${SEARXNG_BASE_URL}",
  "web_search_max_results": 10,
  "web_search_timeout_seconds": 20.0,
  "web_fetch_enabled": true,
  "web_fetch_require_search_result": true,
  "web_fetch_max_chars": 80000,
  "web_fetch_timeout_seconds": 20.0,
  "web_fetch_user_agent": "LuminaCode/1.0"
}
EOF
      chmod 0600 "$DEFAULTS_FILE" 2>/dev/null || true
    fi
  fi
}

compose() {
  cmd="$(compose_cmd)"
  # shellcheck disable=SC2086
  $cmd -f "$COMPOSE_FILE" -p lumina-searxng "$@"
}

healthcheck() {
  url="${SEARXNG_BASE_URL}/search?q=lumina&format=json"
  tries=0
  while [ "$tries" -lt 30 ]; do
    if command -v curl >/dev/null 2>&1 && curl -fsS "$url" >/dev/null 2>&1; then
      log "SearxNG JSON API is ready: $url"
      return 0
    fi
    tries=$((tries + 1))
    sleep 1
  done
  die "SearxNG did not pass JSON API healthcheck: $url"
}

case "$ACTION" in
  install)
    write_settings
    select_image
    write_compose
    merge_defaults
    compose up -d
    healthcheck
    log "Installed SearxNG files under $SEARXNG_ROOT"
    ;;
  configure)
    write_settings
    write_compose
    merge_defaults
    log "Configured SearxNG files under $SEARXNG_ROOT"
    log "Run: $0 install"
    ;;
  start)
    [ -f "$COMPOSE_FILE" ] || { write_settings; select_image; write_compose; merge_defaults; }
    compose up -d
    healthcheck
    ;;
  stop)
    if [ -f "$COMPOSE_FILE" ]; then
      compose down
    else
      log "No SearxNG compose file found at $COMPOSE_FILE"
    fi
    ;;
  restart)
    "$0" stop
    "$0" start
    ;;
  status)
    if [ -f "$COMPOSE_FILE" ]; then
      compose ps
    else
      log "No SearxNG compose file found at $COMPOSE_FILE"
    fi
    ;;
  logs)
    [ -f "$COMPOSE_FILE" ] || die "No SearxNG compose file found at $COMPOSE_FILE"
    compose logs -f
    ;;
  uninstall)
    if [ -f "$COMPOSE_FILE" ]; then
      compose down -v
    fi
    rm -rf "$SEARXNG_ROOT"
    log "Removed $SEARXNG_ROOT"
    ;;
  *)
    die "Usage: $0 {install|configure|start|stop|restart|status|logs|uninstall}"
    ;;
esac
