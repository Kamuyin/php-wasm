#!/usr/bin/env bash
# Docker build entrypoint — executed inside the builder container.
# Runs the full build pipeline and writes artifacts to $OUTPUT_DIR.
set -euo pipefail

PHP_VERSION="${PHP_VERSION:-8.3}"
PROFILE="${PROFILE:-default}"
OUTPUT_DIR="${OUTPUT_DIR:-/out}"
RUN_WASM_OPT="${RUN_WASM_OPT:-0}"
BUILD_CACHE_DIR="${BUILD_CACHE_DIR:-/tmp/php-src-cache}"

echo "==================================================="
echo " php-wasm Docker Build"
echo " PHP Version : ${PHP_VERSION}"
echo " Profile     : ${PROFILE}"
echo " Output dir  : ${OUTPUT_DIR}"
echo " wasm-opt    : ${RUN_WASM_OPT}"
echo " WASI SDK    : ${WASI_SDK_PATH:-/opt/wasi-sdk}"
echo " Build cache : ${BUILD_CACHE_DIR}"
echo "==================================================="
echo ""

mkdir -p "${OUTPUT_DIR}" "${BUILD_CACHE_DIR}"

BUILD_ARGS=(
    "${PHP_VERSION}"
    "${PROFILE}"
    "--output-dir" "${OUTPUT_DIR}"
    "--jobs" "$(nproc)"
)

if [[ "${RUN_WASM_OPT}" != "1" ]]; then
    BUILD_ARGS+=("--no-wasm-opt")
fi

/build/scripts/build.sh "${BUILD_ARGS[@]}"

echo ""
echo "==> Final artifacts:"
ls -lh "${OUTPUT_DIR}"/*.wasm "${OUTPUT_DIR}"/*.sha256 2>/dev/null || \
    echo "  (no artifacts found — build may have failed)"
