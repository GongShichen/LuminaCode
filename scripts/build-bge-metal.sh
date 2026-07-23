#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
OUTPUT="${1:-$REPO_ROOT/tmp/lumina-bge-metal}"

if [ "$(uname -s)" != "Darwin" ] || [ "$(uname -m)" != "arm64" ]; then
    exit 0
fi

command -v xcodebuild >/dev/null 2>&1 || {
    printf 'BGE-M3 Metal build requires Xcode with xcodebuild\n' >&2
    exit 1
}
command -v xcrun >/dev/null 2>&1 || {
    printf 'BGE-M3 Metal build requires Xcode with xcrun\n' >&2
    exit 1
}
xcrun --sdk macosx metal -v >/dev/null 2>&1 || {
    printf 'BGE-M3 Metal build requires the Apple Metal Toolchain; rerun make install so preflight can prepare it\n' >&2
    exit 1
}

PACKAGE="$REPO_ROOT/memory/localmodel/metalrunner"
SCRATCH="$REPO_ROOT/tmp/swift-bge-metal"
(
    cd "$PACKAGE"
    xcodebuild \
        -scheme LuminaBGEMetal \
        -destination 'platform=macOS,arch=arm64' \
        -configuration Release \
        -clonedSourcePackagesDirPath "$SCRATCH/packages" \
        -derivedDataPath "$SCRATCH/derived" \
        -skipPackagePluginValidation \
        -quiet \
        build
)

PRODUCT="$SCRATCH/derived/Build/Products/Release/lumina-bge-metal"
BUNDLE="$SCRATCH/derived/Build/Products/Release/mlx-swift_Cmlx.bundle"
test -x "$PRODUCT"
test -f "$BUNDLE/Contents/Resources/default.metallib"
mkdir -p "$(dirname "$OUTPUT")"
install -m 0755 "$PRODUCT" "$OUTPUT"
rm -rf "$(dirname "$OUTPUT")/mlx-swift_Cmlx.bundle"
cp -R "$BUNDLE" "$(dirname "$OUTPUT")/mlx-swift_Cmlx.bundle"
