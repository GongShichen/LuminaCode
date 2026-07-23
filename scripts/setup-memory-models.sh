#!/usr/bin/env sh
set -eu

ACTION="${1:-install}"
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$SCRIPT_DIR/app-paths.sh"
APP_ROOT="$(lumina_resolve_app_root "${LUMINA_APP_ROOT:-${APP_ROOT:-}}")"
LOCK_FILE="${LUMINA_MEMORY_MODELS_LOCK:-$SCRIPT_DIR/memory-models.lock}"
BACKEND="${LUMINA_BACKEND_BIN:-$APP_ROOT/app/bin/lumina-backend}"
ENDPOINT="${LUMINA_MODELSCOPE_ENDPOINT:-https://modelscope.cn}"
ENDPOINT="${ENDPOINT%/}"
MODELS_ROOT="$APP_ROOT/cache/models"
MEMORY_ROOT="$MODELS_ROOT/memory"
STAGING_ROOT="$MODELS_ROOT/.staging"
MIN_FREE_KIB="${LUMINA_MEMORY_MODELS_MIN_FREE_KIB:-5242880}"
DOWNLOAD_RETRIES="${LUMINA_MODEL_DOWNLOAD_RETRIES:-5}"
DOWNLOAD_RETRY_DELAY="${LUMINA_MODEL_DOWNLOAD_RETRY_DELAY:-2}"
ORT_RELEASE=1.26.0
METAL_RUNTIME_BIN="${LUMINA_BGE_METAL_BIN:-}"

fail() {
    printf 'Memory model setup failed: %s\n' "$*" >&2
    exit 1
}

sha256_file() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        fail "a SHA-256 command is required (shasum or sha256sum)"
    fi
}

file_size() {
    if stat -f '%z' "$1" >/dev/null 2>&1; then
        stat -f '%z' "$1"
    else
        stat -c '%s' "$1"
    fi
}

verify_file() {
    verify_path="$1"
    verify_size="$2"
    verify_sha="$3"
    [ -f "$verify_path" ] || return 1
    [ "$(file_size "$verify_path")" = "$verify_size" ] || return 1
    [ "$(sha256_file "$verify_path")" = "$verify_sha" ]
}

model_revision() {
    awk -F '|' -v model="$1" '$1 == model { print $3; exit }' "$LOCK_FILE"
}

model_repository() {
    awk -F '|' -v model="$1" '$1 == model { print $2; exit }' "$LOCK_FILE"
}

selected_model_variant() {
    configured="$(printf '%s' "${LUMINA_MEMORY_MODEL_VARIANT:-}" | tr '[:upper:]' '[:lower:]')"
    case "$configured" in
        cpu-int8|accelerator-fp16|metal-int8) printf '%s\n' "$configured"; return ;;
        '') ;;
        *) fail "unsupported LUMINA_MEMORY_MODEL_VARIANT: $configured" ;;
    esac
    device="$(printf '%s' "${LUMINA_MEMORY_EMBEDDING_DEVICE:-auto}" | tr '[:upper:]' '[:lower:]')"
    case "$device" in
        cpu) printf '%s\n' cpu-int8; return ;;
        metal)
            case "$(uname -s)/$(uname -m)" in
                Darwin/arm64) printf '%s\n' metal-int8; return ;;
                *) fail "the Metal BGE-M3 runtime requires macOS on Apple Silicon" ;;
            esac
            ;;
        coreml|cuda|nvidia|directml|dml|migraphx|rocm|amd)
            printf '%s\n' accelerator-fp16
            return
            ;;
        auto|'') ;;
        *) fail "unsupported LUMINA_MEMORY_EMBEDDING_DEVICE: $device" ;;
    esac
    case "$(uname -s)/$(uname -m)" in
        Darwin/arm64)
            printf '%s\n' metal-int8
            ;;
        Linux/x86_64|Linux/amd64)
            if [ -e /dev/nvidiactl ] || command -v nvidia-smi >/dev/null 2>&1 ||
                [ -e /dev/kfd ] || command -v rocm-smi >/dev/null 2>&1; then
                printf '%s\n' accelerator-fp16
            else
                printf '%s\n' cpu-int8
            fi
            ;;
        *) printf '%s\n' cpu-int8 ;;
    esac
}

nearest_existing_directory() {
    candidate="$1"
    while [ ! -d "$candidate" ]; do
        parent="$(dirname "$candidate")"
        [ "$parent" != "$candidate" ] || break
        candidate="$parent"
    done
    printf '%s\n' "$candidate"
}

lock_profile_selected() {
    profile="${1:-common}"
    variant="$2"
    [ -z "$profile" ] || [ "$profile" = "common" ] || [ "$profile" = "$variant" ]
}

model_variant_repository() {
    awk -F '|' -v model="$1" -v profile="$2" \
        '$1 == model && $8 == profile { print $2; exit }' "$LOCK_FILE"
}

model_variant_revision() {
    awk -F '|' -v model="$1" -v profile="$2" \
        '$1 == model && $8 == profile { print $3; exit }' "$LOCK_FILE"
}

verify_model_manifest() {
    model="$1"
    root="$2"
    variant="$3"
    manifest="$root/manifest.json"
    revision="$(model_revision "$model")"
    repository="$(model_repository "$model")"
    variant_repository="$(model_variant_repository "$model" "$variant")"
    variant_revision="$(model_variant_revision "$model" "$variant")"
    [ -n "$variant_repository" ] || variant_repository="$repository"
    [ -n "$variant_revision" ] || variant_revision="$revision"
    [ -f "$manifest" ] || return 1
    grep -F -q "\"model\": \"$model\"" "$manifest" &&
        grep -F -q "\"repository\": \"$repository\"" "$manifest" &&
        grep -F -q "\"revision\": \"$revision\"" "$manifest" &&
        grep -F -q "\"variant\": \"$variant\"" "$manifest" &&
        grep -F -q "\"variant_repository\": \"$variant_repository\"" "$manifest" &&
        grep -F -q "\"variant_revision\": \"$variant_revision\"" "$manifest"
}

verify_locked_model() {
    model="$1"
    root="$2"
    variant="$3"
    found=0
    while IFS='|' read -r lock_model lock_repository lock_revision lock_remote lock_local lock_size lock_sha lock_profile; do
        case "$lock_model" in ''|'#'*) continue ;; esac
        [ "$lock_model" = "$model" ] || continue
        lock_profile_selected "$lock_profile" "$variant" || continue
        found=1
        if ! verify_file "$root/$lock_local" "$lock_size" "$lock_sha"; then
            printf 'Invalid model asset: %s\n' "$root/$lock_local" >&2
            return 1
        fi
    done < "$LOCK_FILE"
    [ "$found" -eq 1 ]
}

download_locked_model() {
    model="$1"
    root="$2"
    variant="$3"
    while IFS='|' read -r lock_model lock_repository lock_revision lock_remote lock_local lock_size lock_sha lock_profile; do
        case "$lock_model" in ''|'#'*) continue ;; esac
        [ "$lock_model" = "$model" ] || continue
        lock_profile_selected "$lock_profile" "$variant" || continue
        target="$root/$lock_local"
        if verify_file "$target" "$lock_size" "$lock_sha" 2>/dev/null; then
            continue
        fi
        mkdir -p "$(dirname "$target")"
        partial="$target.partial"
        if [ -f "$partial" ] && [ "$(file_size "$partial")" -gt "$lock_size" ]; then
            rm -f "$partial"
        fi
        url="$ENDPOINT/models/$lock_repository/resolve/$lock_revision/$lock_remote"
        printf 'Downloading %s (%s bytes)\n' "$url" "$lock_size"
		curl -fL --retry "$DOWNLOAD_RETRIES" --retry-all-errors --retry-delay "$DOWNLOAD_RETRY_DELAY" --connect-timeout 20 \
            -C - -o "$partial" "$url"
        verify_file "$partial" "$lock_size" "$lock_sha" || fail "checksum or size mismatch for $lock_remote"
        chmod 0600 "$partial"
        mv "$partial" "$target"
    done < "$LOCK_FILE"
}

runtime_descriptor() {
    os="$(uname -s)"
    arch="$(uname -m)"
    variant="$1"
    device="$(printf '%s' "${LUMINA_MEMORY_EMBEDDING_DEVICE:-auto}" | tr '[:upper:]' '[:lower:]')"
    case "$os/$arch" in
        Darwin/arm64)
            if [ "$variant" = "metal-int8" ]; then
                printf '%s\n' "native-metal|||metal"
                return
            fi
            provider=cpu
            if [ "$variant" = "accelerator-fp16" ]; then provider=coreml; fi
            printf '%s\n' "onnxruntime-osx-arm64-$ORT_RELEASE.tgz|7a1280bbb1701ea514f71828765237e7896e0f2e1cd332f1f70dbd5c3e33aca3|lib/libonnxruntime.dylib|$provider"
            ;;
        Linux/x86_64|Linux/amd64)
            if [ "$variant" = "accelerator-fp16" ] &&
                { [ "$device" = "cuda" ] || [ "$device" = "nvidia" ] ||
                    [ -e /dev/nvidiactl ] || command -v nvidia-smi >/dev/null 2>&1; }; then
                printf '%s\n' "onnxruntime-linux-x64-gpu-$ORT_RELEASE.tgz|cb7df7ee2ca0f962c7ce7c839aeae36223d146a91fb4646d62fb0046f297479f|lib/libonnxruntime.so.$ORT_RELEASE|cuda"
            else
                printf '%s\n' "onnxruntime-linux-x64-$ORT_RELEASE.tgz|1254da24fb389cf39dc0ff3451ab48301740ffbfcbaf646849df92f80ee92c57|lib/libonnxruntime.so.$ORT_RELEASE|cpu"
            fi
            ;;
        Linux/aarch64|Linux/arm64)
            printf '%s\n' "onnxruntime-linux-aarch64-$ORT_RELEASE.tgz|34ff1c2d0f12e2cf3d33a0c5f82e39792e1d581fbd6968fd7c30d173654be01a|lib/libonnxruntime.so.$ORT_RELEASE|cpu"
            ;;
        *) fail "unsupported memory model platform: $os/$arch" ;;
    esac
}

runtime_ready() {
    root="$1"
    [ -s "$root/runtime/provider" ] || return 1
    provider="$(tr -d '[:space:]' < "$root/runtime/provider")"
    if [ "$provider" = "metal" ]; then
        [ -x "$root/runtime/bin/lumina-bge-metal" ] &&
            [ -f "$root/runtime/bin/mlx-swift_Cmlx.bundle/Contents/Resources/default.metallib" ]
        return
    fi
    case "$(uname -s)" in
        Darwin) library="$root/runtime/lib/libonnxruntime.dylib" ;;
        *) library="$root/runtime/lib/libonnxruntime.so.1" ;;
    esac
    [ -f "$library" ] && [ -s "$root/runtime/provider" ]
}

runtime_matches_variant() {
    root="$1"
    variant="$2"
    runtime_ready "$root" || return 1
    descriptor="$(runtime_descriptor "$variant")"
    expected_provider="$(printf '%s' "$descriptor" | cut -d'|' -f4)"
    [ "$(tr -d '[:space:]' < "$root/runtime/provider")" = "$expected_provider" ]
}

install_runtime() {
	runtime_target_root="$1"
    variant="$2"
	runtime_matches_variant "$runtime_target_root" "$variant" && return 0
    if [ "$variant" = "metal-int8" ]; then
        [ "$(uname -s)/$(uname -m)" = "Darwin/arm64" ] ||
            fail "the Metal BGE-M3 runtime requires macOS on Apple Silicon"
        [ -n "$METAL_RUNTIME_BIN" ] && [ -x "$METAL_RUNTIME_BIN" ] ||
            fail "the built Metal runtime is missing; expected LUMINA_BGE_METAL_BIN to be executable"
        metal_bundle="$(dirname "$METAL_RUNTIME_BIN")/mlx-swift_Cmlx.bundle"
        [ -f "$metal_bundle/Contents/Resources/default.metallib" ] ||
            fail "the built Metal runtime bundle is missing default.metallib"
        rm -rf "$runtime_target_root/runtime"
        mkdir -p "$runtime_target_root/runtime/bin"
        install -m 0700 "$METAL_RUNTIME_BIN" "$runtime_target_root/runtime/bin/lumina-bge-metal"
        cp -R "$metal_bundle" "$runtime_target_root/runtime/bin/mlx-swift_Cmlx.bundle"
        printf '%s\n' metal > "$runtime_target_root/runtime/provider"
        return
    fi
    descriptor="$(runtime_descriptor "$variant")"
    archive_name="$(printf '%s' "$descriptor" | cut -d'|' -f1)"
    archive_sha="$(printf '%s' "$descriptor" | cut -d'|' -f2)"
    library_path="$(printf '%s' "$descriptor" | cut -d'|' -f3)"
    provider="$(printf '%s' "$descriptor" | cut -d'|' -f4)"
    archive="$runtime_target_root/$archive_name"
    partial="$archive.partial"
    if ! verify_file "$archive" "$(file_size "$archive" 2>/dev/null || printf 0)" "$archive_sha" 2>/dev/null; then
        rm -f "$archive"
		curl -fL --retry "$DOWNLOAD_RETRIES" --retry-all-errors --retry-delay "$DOWNLOAD_RETRY_DELAY" --connect-timeout 20 -C - \
            -o "$partial" "https://github.com/microsoft/onnxruntime/releases/download/v$ORT_RELEASE/$archive_name"
        [ "$(sha256_file "$partial")" = "$archive_sha" ] || fail "checksum mismatch for $archive_name"
        mv "$partial" "$archive"
    fi
    extract="$runtime_target_root/.runtime-extract"
    rm -rf "$extract"
    mkdir -p "$extract" "$runtime_target_root/runtime/lib"
    tar -xzf "$archive" -C "$extract"
    source_library="$(find "$extract" -type f -path "*/$library_path" -print -quit)"
    [ -n "$source_library" ] || fail "ONNX Runtime library not found in $archive_name"
    case "$(uname -s)" in
        Darwin) target_library="$runtime_target_root/runtime/lib/libonnxruntime.dylib" ;;
        *) target_library="$runtime_target_root/runtime/lib/libonnxruntime.so.1" ;;
    esac
    cp "$source_library" "$target_library"
    if [ "$provider" = "cuda" ]; then
        find "$extract" -type f -name 'libonnxruntime_providers_*.so' -exec cp '{}' "$runtime_target_root/runtime/lib/" ';'
    fi
    printf '%s\n' "$provider" > "$runtime_target_root/runtime/provider"
    rm -rf "$extract" "$archive"
}

all_models_ready() {
	variant="$(selected_model_variant)"
	verify_locked_model bge-m3 "$MEMORY_ROOT/bge-m3" "$variant" >/dev/null 2>&1 &&
		verify_model_manifest bge-m3 "$MEMORY_ROOT/bge-m3" "$variant" >/dev/null 2>&1 &&
		runtime_matches_variant "$MEMORY_ROOT/bge-m3" "$variant" &&
		"$BACKEND" models verify-bge-heads --model-dir "$MEMORY_ROOT/bge-m3" >/dev/null 2>&1 &&
		"$BACKEND" models probe-bge --model-dir "$MEMORY_ROOT/bge-m3" >/dev/null 2>&1
}

check_space() {
    mkdir -p "$MODELS_ROOT"
    available="$(df -Pk "$MODELS_ROOT" | awk 'NR == 2 {print $4}')"
    [ -n "$available" ] || fail "could not determine free disk space"
    [ "$available" -ge "$MIN_FREE_KIB" ] || fail "at least 5 GiB free space is required under $MODELS_ROOT"
}

check_space_readonly() {
    probe_root="$(nearest_existing_directory "$MODELS_ROOT")"
    available="$(df -Pk "$probe_root" | awk 'NR == 2 {print $4}')"
    [ -n "$available" ] || fail "could not determine free disk space"
    [ "$available" -ge "$MIN_FREE_KIB" ] ||
        fail "at least 5 GiB free space is required under $MODELS_ROOT"
}

model_files_ready_without_backend() {
    variant="$1"
    verify_locked_model bge-m3 "$MEMORY_ROOT/bge-m3" "$variant" >/dev/null 2>&1 &&
        verify_model_manifest bge-m3 "$MEMORY_ROOT/bge-m3" "$variant" >/dev/null 2>&1 &&
        runtime_matches_variant "$MEMORY_ROOT/bge-m3" "$variant"
}

validate_endpoint() {
    case "$ENDPOINT" in
        https://*) ;;
        http://127.0.0.1:*|http://localhost:*)
            [ "${LUMINA_ALLOW_INSECURE_MODEL_ENDPOINT:-0}" = "1" ] ||
                fail "an HTTPS ModelScope endpoint is required"
            ;;
        *) fail "an HTTPS ModelScope endpoint is required" ;;
    esac
}

preflight_models() {
    command -v curl >/dev/null 2>&1 || fail "curl is required"
    command -v tar >/dev/null 2>&1 || fail "tar is required"
    [ -r "$LOCK_FILE" ] || fail "model lock is missing: $LOCK_FILE"
    validate_endpoint
    variant="$(selected_model_variant)"
    descriptor="$(runtime_descriptor "$variant")"
    provider="$(printf '%s' "$descriptor" | cut -d'|' -f4)"
    [ -n "$(model_variant_repository bge-m3 "$variant")" ] ||
        fail "model lock has no BGE-M3 assets for profile $variant"

    if model_files_ready_without_backend "$variant"; then
        source_status="verified local assets"
    else
        check_space_readonly
        if [ "${LUMINA_INSTALL_PREFLIGHT_OFFLINE:-0}" != "1" ]; then
            curl -fsIL --retry 1 --connect-timeout 10 --max-time 20 \
                "$ENDPOINT/" >/dev/null ||
                fail "ModelScope endpoint is unreachable: $ENDPOINT"
        fi
        source_status="download required"
    fi

    printf '  memory model: BGE-M3 profile=%s provider=%s (%s)\n' \
        "$variant" "$provider" "$source_status"
}

preflight_installed_models() {
    [ -r "$LOCK_FILE" ] || fail "model lock is missing: $LOCK_FILE"
    variant="$(selected_model_variant)"
    model_files_ready_without_backend "$variant" ||
        fail "SKIP_MEMORY_MODELS=1 requires verified preinstalled BGE-M3 assets for $variant"
    printf '  memory model: BGE-M3 profile=%s (verified preinstalled assets)\n' "$variant"
}

write_model_manifest() {
    manifest_model="$1"
    manifest_root="$2"
    manifest_variant="$3"
    manifest_revision="$(model_revision "$manifest_model")"
    manifest_repository="$(model_repository "$manifest_model")"
    manifest_variant_repository="$(model_variant_repository "$manifest_model" "$manifest_variant")"
    manifest_variant_revision="$(model_variant_revision "$manifest_model" "$manifest_variant")"
    [ -n "$manifest_variant_repository" ] || manifest_variant_repository="$manifest_repository"
    [ -n "$manifest_variant_revision" ] || manifest_variant_revision="$manifest_revision"
    manifest_temp="$manifest_root/manifest.json.tmp"
    cat > "$manifest_temp" <<EOF
{
  "model": "$manifest_model",
  "repository": "$manifest_repository",
  "revision": "$manifest_revision",
  "variant": "$manifest_variant",
  "variant_repository": "$manifest_variant_repository",
  "variant_revision": "$manifest_variant_revision",
  "endpoint": "$ENDPOINT",
  "lock": "memory-models.lock"
}
EOF
    chmod 0600 "$manifest_temp"
    mv "$manifest_temp" "$manifest_root/manifest.json"
}

model_ready() {
    model="$1"
    root="$2"
    variant="$3"
	verify_locked_model "$model" "$root" "$variant" >/dev/null 2>&1 || return 1
	verify_model_manifest "$model" "$root" "$variant" >/dev/null 2>&1 || return 1
	runtime_matches_variant "$root" "$variant" &&
		"$BACKEND" models verify-bge-heads --model-dir "$root" >/dev/null 2>&1 &&
		"$BACKEND" models probe-bge --model-dir "$root" >/dev/null 2>&1
}

model_assets_ready() {
    model="$1"
    root="$2"
    variant="$3"
    verify_locked_model "$model" "$root" "$variant" >/dev/null 2>&1 || return 1
	runtime_matches_variant "$root" "$variant" &&
		"$BACKEND" models verify-bge-heads --model-dir "$root" >/dev/null 2>&1
}

clone_model_tree() {
    source_root="$1"
    target_root="$2"
    rm -rf "$target_root"
    mkdir -p "$target_root"
    if [ "$(uname -s)" = "Darwin" ] && cp -cR "$source_root/." "$target_root/" 2>/dev/null; then
        return 0
    fi
    if cp -a --reflink=auto "$source_root/." "$target_root/" 2>/dev/null; then
        return 0
    fi
    cp -Rp "$source_root/." "$target_root/"
}

STAGED_MODELS=""
PUBLISHED_MODELS=""
LOCK_PUBLISHED=0

prepare_model() {
    model="$1"
    variant="$2"
    final="$MEMORY_ROOT/$model"
    if model_ready "$model" "$final" "$variant"; then
        printf '%s (%s) is already verified at %s\n' "$model" "$variant" "$final"
        return 0
    fi
    revision="$(model_revision "$model")"
    stage="$STAGING_ROOT/$model-$revision"
	if [ ! -d "$stage" ]; then
		if [ -d "$final" ]; then
			clone_model_tree "$final" "$stage"
		else
			mkdir -p "$stage"
		fi
    fi
    chmod 0700 "$stage"
	if ! verify_locked_model "$model" "$stage" "$variant" >/dev/null 2>&1; then
		download_locked_model "$model" "$stage" "$variant"
	fi
	rm -f "$stage/onnx/model.onnx_data"
	if [ "$variant" = "metal-int8" ]; then
		rm -f "$stage/onnx/model.onnx"
		rm -rf "$stage/runtime"
	else
		rm -rf "$stage/metal" "$stage/runtime/bin"
	fi
	if [ "$variant" = "cpu-int8" ]; then
		rm -rf "$stage/runtime/coreml-cache"
	fi
	install_runtime "$stage" "$variant"
	"$BACKEND" models prepare-bge-heads --model-dir "$stage"
	"$BACKEND" models verify-bge-heads --model-dir "$stage"
    verify_locked_model "$model" "$stage" "$variant"
    write_model_manifest "$model" "$stage" "$variant"
    find "$stage" -type d -exec chmod 0700 '{}' ';'
    find "$stage" -type f -exec chmod 0600 '{}' ';'
    if [ "$variant" = "metal-int8" ]; then
        chmod 0700 "$stage/runtime/bin/lumina-bge-metal"
    fi
    model_ready "$model" "$stage" "$variant" || fail "staging validation failed for $model ($variant)"
    STAGED_MODELS="$STAGED_MODELS $model"
}

rollback_model_publication() {
    for rollback_model in $PUBLISHED_MODELS; do
        rollback_final="$MEMORY_ROOT/$rollback_model"
        rm -rf "$rollback_final"
    done
    for rollback_model in $STAGED_MODELS; do
        rollback_final="$MEMORY_ROOT/$rollback_model"
        rollback_backup="$STAGING_ROOT/$rollback_model.previous"
        if [ -e "$rollback_backup" ]; then
            rm -rf "$rollback_final"
            mv "$rollback_backup" "$rollback_final" || true
        fi
    done
    if [ "$LOCK_PUBLISHED" -eq 1 ]; then
        rm -f "$MEMORY_ROOT/models.lock"
    fi
    if [ -e "$STAGING_ROOT/models.lock.previous" ]; then
        mv "$STAGING_ROOT/models.lock.previous" "$MEMORY_ROOT/models.lock" || true
    fi
}

publish_prepared_models() {
    PUBLISHED_MODELS=""
    LOCK_PUBLISHED=0
    lock_stage="$STAGING_ROOT/models.lock.new"
    cp "$LOCK_FILE" "$lock_stage"
    chmod 0600 "$lock_stage"
    for publish_model in $STAGED_MODELS; do
        publish_final="$MEMORY_ROOT/$publish_model"
        publish_backup="$STAGING_ROOT/$publish_model.previous"
        rm -rf "$publish_backup"
        if [ -e "$publish_final" ]; then
            if ! mv "$publish_final" "$publish_backup"; then
                rollback_model_publication
                fail "could not stage the existing $publish_model for replacement"
            fi
        fi
    done
    if [ -e "$MEMORY_ROOT/models.lock" ]; then
        rm -f "$STAGING_ROOT/models.lock.previous"
        if ! mv "$MEMORY_ROOT/models.lock" "$STAGING_ROOT/models.lock.previous"; then
            rollback_model_publication
            fail "could not stage the existing model lock for replacement"
        fi
    fi
    for publish_model in $STAGED_MODELS; do
        publish_stage="$STAGING_ROOT/$publish_model-$(model_revision "$publish_model")"
        publish_final="$MEMORY_ROOT/$publish_model"
        if ! mv "$publish_stage" "$publish_final"; then
            rollback_model_publication
            fail "could not publish $publish_model"
        fi
        PUBLISHED_MODELS="$PUBLISHED_MODELS $publish_model"
    done
    if ! mv "$lock_stage" "$MEMORY_ROOT/models.lock"; then
        rollback_model_publication
        fail "could not publish the model lock"
    fi
    LOCK_PUBLISHED=1
    for publish_model in $STAGED_MODELS; do
        rm -rf "$STAGING_ROOT/$publish_model.previous"
    done
    rm -f "$STAGING_ROOT/models.lock.previous"
}

install_models() {
    command -v curl >/dev/null 2>&1 || fail "curl is required"
    [ -r "$LOCK_FILE" ] || fail "model lock is missing: $LOCK_FILE"
    [ -x "$BACKEND" ] || fail "built backend is required: $BACKEND"
    mkdir -p "$MEMORY_ROOT" "$STAGING_ROOT"
    chmod 0700 "$APP_ROOT/cache" "$MODELS_ROOT" "$MEMORY_ROOT" "$STAGING_ROOT" 2>/dev/null || true
	if all_models_ready; then
        if [ ! -f "$MEMORY_ROOT/models.lock" ] || ! cmp -s "$LOCK_FILE" "$MEMORY_ROOT/models.lock"; then
            cp "$LOCK_FILE" "$STAGING_ROOT/models.lock.new"
            chmod 0600 "$STAGING_ROOT/models.lock.new"
            mv "$STAGING_ROOT/models.lock.new" "$MEMORY_ROOT/models.lock"
		fi
		rm -rf "$MEMORY_ROOT/multilingual-e5-small"
		printf 'Memory models already verified under %s\n' "$MEMORY_ROOT"
        return 0
    fi
    check_space
    STAGED_MODELS=""
	variant="$(selected_model_variant)"
	printf 'Selected BGE-M3 profile: %s\n' "$variant"
	prepare_model bge-m3 "$variant"
	publish_prepared_models
	rm -rf "$MEMORY_ROOT/multilingual-e5-small"
    printf 'Memory models ready under %s\n' "$MEMORY_ROOT"
}

doctor_models() {
	[ -x "$BACKEND" ] || fail "backend is required for model diagnostics: $BACKEND"
	variant="$(selected_model_variant)"
	verify_locked_model bge-m3 "$MEMORY_ROOT/bge-m3" "$variant"
	verify_model_manifest bge-m3 "$MEMORY_ROOT/bge-m3" "$variant" || fail "BGE-M3 manifest is invalid"
	runtime_matches_variant "$MEMORY_ROOT/bge-m3" "$variant" || fail "BGE-M3 runtime does not match $variant"
	"$BACKEND" models verify-bge-heads --model-dir "$MEMORY_ROOT/bge-m3"
	"$BACKEND" models probe-bge --model-dir "$MEMORY_ROOT/bge-m3"
	printf 'Memory model profile %s, manifest, linear heads, and inference probe are valid\n' "$variant"
}

case "$ACTION" in
    preflight) preflight_models ;;
    preflight-installed) preflight_installed_models ;;
    install) install_models ;;
    status|doctor) doctor_models ;;
    uninstall)
        rm -rf "$MEMORY_ROOT" "$STAGING_ROOT"
        printf 'Removed managed memory models from %s\n' "$MODELS_ROOT"
        ;;
    *) fail "usage: $0 {preflight|preflight-installed|install|status|doctor|uninstall}" ;;
esac
