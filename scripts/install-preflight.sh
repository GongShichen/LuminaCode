#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
APP_ROOT="${LUMINA_APP_ROOT:-${APP_ROOT:-$HOME/.lumina}}"
SKIP_MANAGED_COMPONENTS="${SKIP_MANAGED_COMPONENTS:-0}"
SKIP_MEMORY_MODELS="${SKIP_MEMORY_MODELS:-0}"
PREPARE_ACCELERATOR="${LUMINA_INSTALL_PREPARE_ACCELERATOR:-1}"

fail() {
    printf 'LuminaCode install preflight failed: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command '$1' was not found in PATH"
}

case "$APP_ROOT" in
    /*) ;;
    *) fail "APP_ROOT must be absolute: $APP_ROOT" ;;
esac
[ "$APP_ROOT" != "/" ] || fail "refusing unsafe APP_ROOT: $APP_ROOT"

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
    Darwin/arm64|Linux/x86_64|Linux/amd64|Linux/aarch64|Linux/arm64) ;;
    *) fail "unsupported platform: $os/$arch" ;;
esac

for command_name in go node npm curl tar awk sed grep find; do
    require_command "$command_name"
done
if ! command -v shasum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
    fail "a SHA-256 command is required (shasum or sha256sum)"
fi
if ! command -v "${CC:-cc}" >/dev/null 2>&1; then
    fail "a C compiler is required for the local BGE-M3 runtime (${CC:-cc})"
fi

prepare_apple_metal() {
    require_command xcodebuild
    require_command xcrun
    require_command swift
    if xcrun --sdk macosx metal -v >/dev/null 2>&1; then
        return
    fi
    [ "$PREPARE_ACCELERATOR" = "1" ] ||
        fail "the Apple Metal Toolchain is missing and automatic preparation is disabled"
    printf '  accelerator setup: downloading the Apple Metal Toolchain with xcodebuild\n'
    if ! xcodebuild -downloadComponent MetalToolchain; then
        fail "Apple Metal Toolchain download failed; check Xcode license, network access, and available disk space"
    fi
    xcrun --sdk macosx metal -v >/dev/null 2>&1 ||
        fail "Apple Metal Toolchain was downloaded but xcrun cannot execute the metal compiler"
}

if [ "$os/$arch" = "Darwin/arm64" ]; then
    prepare_apple_metal
fi

go_version="$(go version 2>/dev/null || true)"
node_version="$(node --version 2>/dev/null || true)"
npm_version="$(npm --version 2>/dev/null || true)"
[ -n "$go_version" ] || fail "Go is installed but could not run"
[ -n "$node_version" ] || fail "Node.js is installed but could not run"
[ -n "$npm_version" ] || fail "npm is installed but could not run"

memory_bytes=""
hardware=""
accelerator="none"
case "$os" in
    Darwin)
        require_command sysctl
        hardware="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || printf 'Apple Silicon')"
        memory_bytes="$(sysctl -n hw.memsize 2>/dev/null || true)"
        accelerator="Metal"
        ;;
    Linux)
        hardware="$(awk -F: '/model name|Hardware/ { sub(/^[[:space:]]+/, "", $2); print $2; exit }' /proc/cpuinfo 2>/dev/null || true)"
        memory_kib="$(awk '/MemTotal:/ { print $2; exit }' /proc/meminfo 2>/dev/null || true)"
        if [ -n "$memory_kib" ]; then memory_bytes=$((memory_kib * 1024)); fi
        if [ -e /dev/nvidiactl ] || command -v nvidia-smi >/dev/null 2>&1; then
            accelerator="CUDA"
        elif [ -e /dev/kfd ] || command -v rocm-smi >/dev/null 2>&1; then
            accelerator="ROCm"
        fi
        ;;
esac

printf 'LuminaCode install preflight\n'
printf '  platform: %s/%s\n' "$os" "$arch"
printf '  hardware: %s\n' "${hardware:-unknown}"
printf '  accelerator: %s\n' "$accelerator"
if [ -n "$memory_bytes" ]; then
    printf '  memory: %s MiB\n' "$((memory_bytes / 1024 / 1024))"
fi
printf '  toolchain: %s; node %s; npm %s; cc %s\n' \
    "$go_version" "$node_version" "$npm_version" "${CC:-cc}"
if [ "$os/$arch" = "Darwin/arm64" ]; then
    metal_version="$(xcrun --sdk macosx metal -v 2>&1 | sed -n '1p')"
    printf '  Metal compiler: %s\n' "${metal_version:-available}"
fi

if [ "$SKIP_MANAGED_COMPONENTS" = "1" ]; then
    printf '  managed memory runtime: skipped (SKIP_MANAGED_COMPONENTS=1)\n'
    exit 0
fi

model_action=preflight
if [ "$SKIP_MEMORY_MODELS" = "1" ]; then model_action=preflight-installed; fi
LUMINA_APP_ROOT="$APP_ROOT" \
    LUMINA_MEMORY_MODELS_LOCK="${LUMINA_MEMORY_MODELS_LOCK:-$SCRIPT_DIR/memory-models.lock}" \
    "$SCRIPT_DIR/setup-memory-models.sh" "$model_action"

printf 'Preflight complete; build and installation may proceed from %s\n' "$REPO_ROOT"
