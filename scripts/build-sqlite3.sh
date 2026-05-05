#!/usr/bin/env bash
# Cross-compiles the SQLite amalgamation for wasm32-wasi using wasi-sdk.
# Output: <DEPS_DIR>/include/sqlite3.h and <DEPS_DIR>/lib/libsqlite3.a
#
# Usage: build-sqlite3.sh [--deps-dir <dir>] [--sqlite-version <ver>]
#
# After running this, add to your profile's configure_flags:
#   - "--enable-pdo"
#   - "--with-pdo-sqlite=<DEPS_DIR>"
#   - "--with-sqlite3=<DEPS_DIR>"
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DEPS_DIR="${SCRIPT_DIR}/../deps/sqlite3-wasm"
SQLITE_VERSION="3470200"   # 3.47.2 — update as needed
SQLITE_YEAR="2024"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --deps-dir)       DEPS_DIR="$2";       shift 2 ;;
        --sqlite-version) SQLITE_VERSION="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

WASI_SDK="${WASI_SDK_PATH:-/opt/wasi-sdk}"
if [[ ! -d "${WASI_SDK}" ]]; then
    echo "ERROR: wasi-sdk not found at '${WASI_SDK}'. Set WASI_SDK_PATH."
    exit 1
fi

WASI_SYSROOT="${WASI_SDK}/share/wasi-sysroot"
CC="${WASI_SDK}/bin/clang"
AR="${WASI_SDK}/bin/llvm-ar"

BUILD_TMP="${TMPDIR:-/tmp}/sqlite3-wasm-build"
AMALG_DIR="${BUILD_TMP}/sqlite-amalgamation-${SQLITE_VERSION}"
AMALG_ZIP="${BUILD_TMP}/sqlite-amalgamation-${SQLITE_VERSION}.zip"

mkdir -p "${BUILD_TMP}" "${DEPS_DIR}/include" "${DEPS_DIR}/lib"

# Download amalgamation if not cached
if [[ ! -f "${AMALG_ZIP}" ]]; then
    URL="https://www.sqlite.org/${SQLITE_YEAR}/sqlite-amalgamation-${SQLITE_VERSION}.zip"
    echo "==> Downloading SQLite ${SQLITE_VERSION} amalgamation..."
    echo "    URL: ${URL}"
    curl -fsSL -o "${AMALG_ZIP}" "${URL}"
fi

if [[ ! -d "${AMALG_DIR}" ]]; then
    echo "==> Extracting..."
    unzip -q "${AMALG_ZIP}" -d "${BUILD_TMP}"
fi

echo "==> Cross-compiling sqlite3 for wasm32-wasi..."
"${CC}" \
    --sysroot="${WASI_SYSROOT}" \
    --target=wasm32-wasi \
    -O2 \
    -DSQLITE_OMIT_WAL=1 \
    -DSQLITE_THREADSAFE=0 \
    -DSQLITE_OMIT_LOAD_EXTENSION=1 \
    -c "${AMALG_DIR}/sqlite3.c" \
    -o "${BUILD_TMP}/sqlite3.o"

"${AR}" rcs "${DEPS_DIR}/lib/libsqlite3.a" "${BUILD_TMP}/sqlite3.o"
cp "${AMALG_DIR}/sqlite3.h" "${DEPS_DIR}/include/sqlite3.h"

# Generate a pkg-config file so PHP's PKG_CHECK_MODULES finds our WASM build
# instead of the host system libsqlite3 (which has no useful headers for wasm32-wasi).
SQLITE_VER_MAJOR=$(( SQLITE_VERSION / 1000000 ))
SQLITE_VER_MINOR=$(( (SQLITE_VERSION / 1000) % 1000 ))
SQLITE_VER_PATCH=$(( SQLITE_VERSION % 1000 ))
SQLITE_VER_STR="${SQLITE_VER_MAJOR}.${SQLITE_VER_MINOR}.${SQLITE_VER_PATCH}"

mkdir -p "${DEPS_DIR}/lib/pkgconfig"
cat > "${DEPS_DIR}/lib/pkgconfig/sqlite3.pc" << EOF
prefix=${DEPS_DIR}
exec_prefix=\${prefix}
libdir=\${exec_prefix}/lib
includedir=\${prefix}/include

Name: SQLite
Description: SQL database engine (wasm32-wasi cross-compiled build)
Version: ${SQLITE_VER_STR}
Libs: -L\${libdir} -lsqlite3
Cflags: -I\${includedir}
EOF

echo ""
echo "==> SQLite built for wasm32-wasi"
echo "    Headers : ${DEPS_DIR}/include/sqlite3.h"
echo "    Library : ${DEPS_DIR}/lib/libsqlite3.a"
echo ""
echo "Add to your profile's configure_flags:"
echo '    - "--enable-pdo"'
echo "    - \"--with-pdo-sqlite=${DEPS_DIR}\""
echo "    - \"--with-sqlite3=${DEPS_DIR}\""
