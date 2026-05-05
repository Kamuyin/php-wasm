#!/usr/bin/env bash
# Usage: build.sh <version> <profile> [--output-dir ./out] [--no-wasm-opt] [--jobs N]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

print_usage() {
    echo "Usage: $0 <version> <profile> [--output-dir <dir>] [--no-wasm-opt] [--jobs N]"
    echo ""
    echo "  version       PHP version: 8.2, 8.3, or 8.4"
    echo "  profile       Build profile: minimal, default, full, honeypot"
    echo "  --output-dir  Output directory (default: ./out)"
    echo "  --no-wasm-opt Skip wasm-opt even when it is available"
    echo "  --jobs N      Parallel make jobs (default: nproc)"
    exit 1
}

VERSION="${1:-}"
PROFILE="${2:-}"
OUTPUT_DIR="${REPO_ROOT}/out"
RUN_WASM_OPT=1
JOBS=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)

if [[ -z "${VERSION}" || -z "${PROFILE}" ]]; then
    print_usage
fi

shift 2
while [[ $# -gt 0 ]]; do
    case "$1" in
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        --no-wasm-opt) RUN_WASM_OPT=0; shift ;;
        --jobs)       JOBS="$2"; shift 2 ;;
        *) echo "ERROR: Unknown option: $1"; print_usage ;;
    esac
done

VERSION_DIR="${REPO_ROOT}/versions/${VERSION}"
if [[ ! -d "${VERSION_DIR}" ]]; then
    echo "ERROR: Unknown PHP version '${VERSION}'. Available: $(ls "${REPO_ROOT}/versions/")"
    exit 1
fi

PROFILE_FILE="${REPO_ROOT}/profiles/${PROFILE}.yaml"
if [[ ! -f "${PROFILE_FILE}" ]]; then
    echo "ERROR: Unknown profile '${PROFILE}'. Available profiles:"
    ls "${REPO_ROOT}/profiles/" | sed 's/\.yaml$/    /'
    exit 1
fi

PHP_VERSION=$(grep '^php_version:' "${VERSION_DIR}/config.yaml" \
    | sed 's/php_version: *"//' | sed 's/"//')
if [[ -z "${PHP_VERSION}" ]]; then
    echo "ERROR: Could not parse php_version from ${VERSION_DIR}/config.yaml"
    exit 1
fi

echo "==> php-wasm build"
echo "    PHP version : ${PHP_VERSION}"
echo "    Profile     : ${PROFILE}"
echo "    Output dir  : ${OUTPUT_DIR}"
echo "    Jobs        : ${JOBS}"
echo "    wasm-opt    : ${RUN_WASM_OPT} (auto if installed)"
echo ""

BUILD_DIR="${TMPDIR:-/tmp}/php-wasm-build-${VERSION}-${PROFILE}"
mkdir -p "${BUILD_DIR}" "${OUTPUT_DIR}"

# Step 1: Fetch source (cache-aware)
echo "==> [1/6] Fetching PHP ${PHP_VERSION} source..."
"${SCRIPT_DIR}/fetch-source.sh" "${VERSION}" "${BUILD_DIR}"

PHP_SRC="${BUILD_DIR}/php-src-php-${PHP_VERSION}"
if [[ ! -d "${PHP_SRC}" ]]; then
    echo "ERROR: Expected source directory not found: ${PHP_SRC}"
    exit 1
fi

# Step 2: Apply patches (idempotent via git state)
echo "==> [2/6] Applying patches..."
"${SCRIPT_DIR}/apply-patches.sh" "${VERSION}" "${PHP_SRC}"

# Step 3: Configure (configure-php.sh runs buildconf --force before ./configure)
echo "==> [3/6] Configuring..."
"${SCRIPT_DIR}/configure-php.sh" "${VERSION}" "${PROFILE}" "${PHP_SRC}"

# Patch generated libtool to disable rpath: wasm-ld doesn't support --rpath and
# libtool's default hardcode_libdir_flag_spec generates "-R /path" for every -L
# passed through pkg-config or --with-*= flags.
sed -i \
    -e 's/^hardcode_libdir_flag_spec=.*/hardcode_libdir_flag_spec=""/' \
    -e 's/^hardcode_libdir_separator=.*/hardcode_libdir_separator=""/' \
    "${PHP_SRC}/libtool"

# Step 4: Build — target 'cgi' only; avoids building CLI/embed which may
# require platform features unavailable in wasm32-wasi (e.g. fork, signals).
echo "==> [4/6] Building (${JOBS} jobs)..."
make -C "${PHP_SRC}" -j"${JOBS}" --no-print-directory cgi 2>&1 | tail -50
echo "    make complete"

WASM_BIN="${PHP_SRC}/sapi/cgi/php-cgi"
if [[ ! -f "${WASM_BIN}" ]]; then
    echo "ERROR: Build succeeded but output binary not found: ${WASM_BIN}"
    echo "  Contents of sapi/cgi/:"
    ls "${PHP_SRC}/sapi/cgi/" 2>/dev/null || true
    exit 1
fi

ARTIFACT_NAME="php-${PHP_VERSION}-${PROFILE}.wasm"
OUTPUT_PATH="${OUTPUT_DIR}/${ARTIFACT_NAME}"

cp "${WASM_BIN}" "${OUTPUT_PATH}"

# Step 5: Optional wasm-opt
if [[ "${RUN_WASM_OPT}" -eq 1 ]] && command -v wasm-opt &>/dev/null; then
    echo "==> [5/6] Running wasm-opt -Oz..."
    TEMP_OPT="${OUTPUT_PATH}.opt"
    wasm-opt -Oz "${OUTPUT_PATH}" -o "${TEMP_OPT}"
    mv "${TEMP_OPT}" "${OUTPUT_PATH}"
    echo "    wasm-opt complete"
else
    echo "==> [5/6] Skipping wasm-opt"
fi

# Step 6: Strip debug info
echo "==> [6/6] Stripping debug symbols..."
if command -v llvm-strip &>/dev/null; then
    TEMP_STRIP="${OUTPUT_PATH}.stripped"
    llvm-strip --strip-debug "${OUTPUT_PATH}" -o "${TEMP_STRIP}"
    mv "${TEMP_STRIP}" "${OUTPUT_PATH}"
    echo "    Stripped with llvm-strip"
elif command -v wasm-strip &>/dev/null; then
    wasm-strip "${OUTPUT_PATH}"
    echo "    Stripped with wasm-strip (wabt)"
else
    echo "    WARNING: No wasm-strip or llvm-strip found; artifact left unstripped"
fi

# Generate sha256 checksum
sha256sum "${OUTPUT_PATH}" > "${OUTPUT_PATH}.sha256"

SIZE=$(du -sh "${OUTPUT_PATH}" | cut -f1)
echo ""
echo "==> Build complete!"
echo "    Artifact : ${OUTPUT_PATH} (${SIZE})"
echo "    SHA256   : $(awk '{print $1}' "${OUTPUT_PATH}.sha256")"
