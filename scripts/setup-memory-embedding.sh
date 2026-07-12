#!/usr/bin/env sh
set -eu

ACTION="${1:-install}"
APP_ROOT="${LUMINA_APP_ROOT:-${APP_ROOT:-$HOME/.lumina}}"
MODEL_NAME="multilingual-e5-small"
MODEL_DIR="$APP_ROOT/models/memory/$MODEL_NAME"
MODELSCOPE_BASE="https://modelscope.cn/models/AI-ModelScope/multilingual-e5-small/resolve/master"
MODEL_URL="$MODELSCOPE_BASE/onnx/model.onnx"
TOKENIZER_URL="$MODELSCOPE_BASE/onnx/tokenizer.json"
MODEL_SHA256="ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
TOKENIZER_SHA256="0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
ORT_RELEASE="1.26.0"

sha256_file() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        echo "A SHA-256 command is required (shasum or sha256sum)." >&2
        return 1
    fi
}

verify_file() {
    file="$1"
    expected="$2"
    [ -f "$file" ] || return 1
    actual="$(sha256_file "$file")"
    [ "$actual" = "$expected" ] || {
        echo "SHA-256 mismatch for $file" >&2
        echo "expected: $expected" >&2
        echo "actual:   $actual" >&2
        return 1
    }
}

download_verified() {
    url="$1"
    target="$2"
    expected="$3"
    if verify_file "$target" "$expected" 2>/dev/null; then
        return 0
    fi
    tmp="$target.download"
    rm -f "$tmp"
    echo "Downloading $url"
    curl -fL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 20 "$url" -o "$tmp"
    verify_file "$tmp" "$expected"
    mv "$tmp" "$target"
}

platform_archive() {
    os="$(uname -s)"
    arch="$(uname -m)"
    device="$(printf '%s' "${LUMINA_MEMORY_EMBEDDING_DEVICE:-auto}" | tr '[:upper:]' '[:lower:]')"
    case "$os/$arch" in
        Darwin/arm64)
            printf '%s\n' "onnxruntime-osx-arm64-$ORT_RELEASE.tgz|7a1280bbb1701ea514f71828765237e7896e0f2e1cd332f1f70dbd5c3e33aca3|lib/libonnxruntime.dylib|coreml"
            ;;
        Linux/x86_64|Linux/amd64)
            if [ "$device" = "cuda" ] || { [ "$device" = "auto" ] && { [ -e /dev/nvidiactl ] || command -v nvidia-smi >/dev/null 2>&1; }; }; then
                printf '%s\n' "onnxruntime-linux-x64-gpu-$ORT_RELEASE.tgz|cb7df7ee2ca0f962c7ce7c839aeae36223d146a91fb4646d62fb0046f297479f|lib/libonnxruntime.so.$ORT_RELEASE|cuda"
            else
                printf '%s\n' "onnxruntime-linux-x64-$ORT_RELEASE.tgz|1254da24fb389cf39dc0ff3451ab48301740ffbfcbaf646849df92f80ee92c57|lib/libonnxruntime.so.$ORT_RELEASE|cpu"
            fi
            ;;
        Linux/aarch64|Linux/arm64)
            printf '%s\n' "onnxruntime-linux-aarch64-$ORT_RELEASE.tgz|34ff1c2d0f12e2cf3d33a0c5f82e39792e1d581fbd6968fd7c30d173654be01a|lib/libonnxruntime.so.$ORT_RELEASE|cpu"
            ;;
        *)
            echo "Unsupported memory embedding platform: $os/$arch" >&2
            return 1
            ;;
    esac
}

install_runtime() {
    descriptor="$(platform_archive)"
    archive_name="$(printf '%s' "$descriptor" | cut -d'|' -f1)"
    archive_sha="$(printf '%s' "$descriptor" | cut -d'|' -f2)"
    library_path="$(printf '%s' "$descriptor" | cut -d'|' -f3)"
    provider="$(printf '%s' "$descriptor" | cut -d'|' -f4)"
    runtime_dir="$MODEL_DIR/runtime"
    case "$(uname -s)" in
        Darwin) target_library="$runtime_dir/lib/libonnxruntime.dylib" ;;
        *) target_library="$runtime_dir/lib/libonnxruntime.so.1" ;;
    esac
    if [ -f "$target_library" ] && [ -f "$runtime_dir/provider" ] && [ "$(cat "$runtime_dir/provider")" = "$provider" ]; then
        return 0
    fi
    archive="$MODEL_DIR/$archive_name"
    download_verified "https://github.com/microsoft/onnxruntime/releases/download/v$ORT_RELEASE/$archive_name" "$archive" "$archive_sha"
    extract_dir="$MODEL_DIR/.runtime-extract"
    rm -rf "$extract_dir"
    mkdir -p "$extract_dir" "$runtime_dir/lib"
    tar -xzf "$archive" -C "$extract_dir"
    source_library="$(find "$extract_dir" -type f -path "*/$library_path" -print -quit)"
    [ -n "$source_library" ] || {
        echo "ONNX Runtime library was not found in $archive_name" >&2
        return 1
    }
    cp "$source_library" "$target_library"
    if [ "$provider" = "cuda" ]; then
        find "$extract_dir" -type f -name 'libonnxruntime_providers_*.so' -exec cp '{}' "$runtime_dir/lib/" ';'
    fi
    printf '%s\n' "$provider" > "$runtime_dir/provider"
    rm -rf "$extract_dir" "$archive"
}

install_embedding() {
    command -v curl >/dev/null 2>&1 || {
        echo "curl is required to install the memory embedding model." >&2
        exit 1
    }
    mkdir -p "$MODEL_DIR"
    download_verified "$MODEL_URL" "$MODEL_DIR/model.onnx" "$MODEL_SHA256"
    download_verified "$TOKENIZER_URL" "$MODEL_DIR/tokenizer.json" "$TOKENIZER_SHA256"
    install_runtime
    cat > "$MODEL_DIR/manifest.json" <<EOF
{
  "model": "$MODEL_NAME",
  "source": "ModelScope/AI-ModelScope/multilingual-e5-small",
  "model_sha256": "$MODEL_SHA256",
  "tokenizer_sha256": "$TOKENIZER_SHA256",
	"runtime_provider": "$(cat "$MODEL_DIR/runtime/provider")",
  "dimensions": 384
}
EOF
    echo "Installed memory embedding assets to $MODEL_DIR"
}

check_embedding() {
    failed=0
    verify_file "$MODEL_DIR/model.onnx" "$MODEL_SHA256" || failed=1
    verify_file "$MODEL_DIR/tokenizer.json" "$TOKENIZER_SHA256" || failed=1
    if [ "$(uname -s)" = "Darwin" ]; then
        [ -f "$MODEL_DIR/runtime/lib/libonnxruntime.dylib" ] || failed=1
    else
        [ -f "$MODEL_DIR/runtime/lib/libonnxruntime.so.1" ] || failed=1
    fi
    if [ "$failed" -ne 0 ]; then
        echo "Memory embedding assets are missing or invalid: $MODEL_DIR" >&2
        return 1
    fi
    echo "Memory embedding assets ready: $MODEL_DIR"
}

case "$ACTION" in
    install) install_embedding ;;
    status|doctor) check_embedding ;;
    uninstall)
        rm -rf "$MODEL_DIR"
        echo "Removed memory embedding assets from $MODEL_DIR"
        ;;
    *)
        echo "Usage: $0 {install|status|doctor|uninstall}" >&2
        exit 2
        ;;
esac
