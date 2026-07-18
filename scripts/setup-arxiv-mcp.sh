#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$SCRIPT_DIR/app-paths.sh"
APP_ROOT="$(lumina_resolve_app_root)"
MCP_ROOT="${LUMINA_ARXIV_MCP_ROOT:-${APP_ROOT}/app/extensions/arxiv-mcp}"
SOURCE_DIR="${MCP_ROOT}/source"
VENV_DIR="${MCP_ROOT}/.venv"
RUNNER_FILE="${MCP_ROOT}/run-arxiv-mcp.py"
CONFIG_FILE="${APP_ROOT}/config/mcp.json"
MANAGED_FILE="${APP_ROOT}/state/managed/mcp.json"
REPO_URL="${LUMINA_ARXIV_MCP_REPO:-https://github.com/kelvingao/arxiv-mcp.git}"
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

python_bin() {
  if command -v python3 >/dev/null 2>&1; then
    command -v python3
    return
  fi
  if command -v python >/dev/null 2>&1; then
    command -v python
    return
  fi
  die "Python 3.11+ is required for arXiv MCP."
}

assert_python_version() {
  py="$1"
  "$py" - <<'PY'
import sys
if sys.version_info < (3, 11):
    raise SystemExit("Python 3.11+ is required, got %s" % ".".join(map(str, sys.version_info[:3])))
PY
}

ensure_uv() {
  if command -v uv >/dev/null 2>&1; then
    command -v uv
    return
  fi
  py="$1"
  "$py" -m pip install --user uv >/dev/null
  if command -v uv >/dev/null 2>&1; then
    command -v uv
    return
  fi
  user_base="$("$py" -m site --user-base)"
  if [ -x "${user_base}/bin/uv" ]; then
    printf '%s\n' "${user_base}/bin/uv"
    return
  fi
  die "uv was installed but could not be found in PATH."
}

venv_python() {
  if [ -x "${VENV_DIR}/bin/python" ]; then
    printf '%s\n' "${VENV_DIR}/bin/python"
  elif [ -x "${VENV_DIR}/Scripts/python.exe" ]; then
    printf '%s\n' "${VENV_DIR}/Scripts/python.exe"
  else
    printf '%s\n' "${VENV_DIR}/bin/python"
  fi
}

venv_arxiv_command() {
  venv_python
}

clone_or_update() {
  mkdir -p "$MCP_ROOT"
  chmod 0755 "$MCP_ROOT" 2>/dev/null || true
  if [ -d "${SOURCE_DIR}/.git" ]; then
    if ! git -C "$SOURCE_DIR" pull --ff-only; then
      warn "Could not update existing arxiv-mcp checkout; continuing with local source at $SOURCE_DIR."
    fi
  elif [ -e "$SOURCE_DIR" ]; then
    die "$SOURCE_DIR exists but is not a git checkout."
  else
    git clone "$REPO_URL" "$SOURCE_DIR"
  fi
}

patch_source_compatibility() {
  server_py="${SOURCE_DIR}/src/server.py"
  if [ ! -f "$server_py" ]; then
    warn "arxiv-mcp server.py not found at $server_py; skipping compatibility patch."
    return
  fi
  SERVER_PY="$server_py" "$(python_bin)" - <<'PY'
import os
from pathlib import Path

path = Path(os.environ["SERVER_PY"])
text = path.read_text(encoding="utf-8")

# kelvingao/arxiv-mcp currently passes FastMCP(description=...), while recent
# mcp.server.fastmcp.FastMCP builds reject that keyword. Keep this patch local
# and idempotent so make install can repair an updated checkout without
# modifying user configuration.
old = '    description="MCP server for retrieving papers from arXiv based on keywords",\n'
if old in text:
    text = text.replace(old, "")
    path.write_text(text, encoding="utf-8")
    print("Patched arxiv-mcp FastMCP description compatibility.")
else:
    print("arxiv-mcp FastMCP compatibility patch already applied or unnecessary.")
PY
}

install_python_env() {
  py="$1"
  uv_bin="$2"
  if [ ! -d "$VENV_DIR" ]; then
    "$py" -m venv "$VENV_DIR"
  fi
  vpy="$(venv_python)"
  "$uv_bin" pip install --python "$vpy" -e "$SOURCE_DIR"
}

write_runner() {
  mkdir -p "$MCP_ROOT"
  cat >"$RUNNER_FILE" <<PY
import asyncio
import pathlib
import sys

source = pathlib.Path(r"$SOURCE_DIR")
sys.path.insert(0, str(source / "src"))

from server import main

asyncio.run(main())
PY
  chmod 0644 "$RUNNER_FILE" 2>/dev/null || true
}

merge_mcp_config() {
	py="$1"
  arxiv_cmd="$(venv_arxiv_command)"
  mkdir -p "$(dirname "$CONFIG_FILE")"
  CONFIG_FILE="$CONFIG_FILE" MANAGED_FILE="$MANAGED_FILE" ARXIV_COMMAND="$arxiv_cmd" RUNNER_FILE="$RUNNER_FILE" SOURCE_DIR="$SOURCE_DIR" "$py" - <<'PY'
import json, os, pathlib, tempfile
config_path = pathlib.Path(os.environ["CONFIG_FILE"])
managed_path = pathlib.Path(os.environ["MANAGED_FILE"])
server = {
    "command": os.environ["ARXIV_COMMAND"],
    "args": [os.environ["RUNNER_FILE"]],
    "env": {"TRANSPORT": "stdio"},
    "cwd": os.environ["SOURCE_DIR"],
}
try:
    data = json.loads(config_path.read_text(encoding="utf-8"))
except Exception:
    data = {}
try:
    managed = json.loads(managed_path.read_text(encoding="utf-8"))
except Exception:
    managed = {}
servers = data.setdefault("mcpServers", {})
existing = servers.get("arxiv")
managed_existing = (managed.get("mcpServers") or {}).get("arxiv")
legacy_command = str((existing or {}).get("command", "")).replace("\\", "/")
owned = existing is None or existing == managed_existing or "/mcp/arxiv-mcp/" in legacy_command
created = False

def atomic_write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(path.parent, 0o700)
    fd, temporary = tempfile.mkstemp(prefix=".lumina-", suffix=".tmp", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(value, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass

if owned:
    servers["arxiv"] = server
    created = True
    atomic_write(config_path, data)
else:
    print("arXiv MCP already exists in mcp.json; leaving user config unchanged.")
if created:
    managed.setdefault("mcpServers", {})["arxiv"] = server
    atomic_write(managed_path, managed)
PY
}

remove_managed_mcp_config() {
  py="$(python_bin)"
  CONFIG_FILE="$CONFIG_FILE" MANAGED_FILE="$MANAGED_FILE" "$py" - <<'PY'
import json, os, pathlib, tempfile

config_path = pathlib.Path(os.environ["CONFIG_FILE"])
managed_path = pathlib.Path(os.environ["MANAGED_FILE"])

def read(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return {}

def atomic_write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(path.parent, 0o700)
    fd, temporary = tempfile.mkstemp(prefix=".lumina-", suffix=".tmp", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(value, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass

config = read(config_path)
managed = read(managed_path)
config_servers = config.get("mcpServers") or {}
managed_servers = managed.get("mcpServers") or {}
current = config_servers.get("arxiv")
owned = managed_servers.get("arxiv")
if owned is not None and current == owned:
    config_servers.pop("arxiv", None)
    config["mcpServers"] = config_servers
    atomic_write(config_path, config)
elif current is not None:
    print("arXiv MCP config was modified by the user; preserving it.")
managed_servers.pop("arxiv", None)
managed["mcpServers"] = managed_servers
if managed_path.exists() or managed_servers:
    atomic_write(managed_path, managed)
PY
}

status() {
  if [ -d "$SOURCE_DIR" ]; then
    log "Source: $SOURCE_DIR"
  else
    log "Source: missing ($SOURCE_DIR)"
  fi
  if [ -x "$(venv_python)" ]; then
    log "Python: $(venv_python)"
  else
    log "Python: missing venv ($VENV_DIR)"
  fi
  if [ -x "$(venv_arxiv_command)" ]; then
    log "Command: $(venv_arxiv_command) $RUNNER_FILE"
  else
    log "Command: missing arxiv entrypoint ($(venv_arxiv_command))"
  fi
  if [ -f "$CONFIG_FILE" ] && grep -q '"arxiv"' "$CONFIG_FILE"; then
    log "MCP config: $CONFIG_FILE contains arxiv"
  else
    log "MCP config: arxiv missing from $CONFIG_FILE"
  fi
}

case "$ACTION" in
  install)
    command -v git >/dev/null 2>&1 || die "git is required for arXiv MCP setup."
    py="$(python_bin)"
    assert_python_version "$py"
	uv_bin="$(ensure_uv "$py")"
	clone_or_update
	patch_source_compatibility
	install_python_env "$py" "$uv_bin"
	write_runner
	merge_mcp_config "$py"
    status
    ;;
  status)
    status
    ;;
  uninstall)
    remove_managed_mcp_config
    warn "Removing managed extension $MCP_ROOT."
    rm -rf "$MCP_ROOT"
    ;;
  *)
    die "Usage: $0 {install|status|uninstall}"
    ;;
esac
