#!/usr/bin/env bash
# Usage: build-lib.sh <lib-name>
# Reads libraries/<lib-name>.yaml, fetches, patches, and cross-compiles for wasm32-wasi.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LIB_NAME="${1:-}"

if [[ -z "${LIB_NAME}" ]]; then
    echo "Usage: $0 <lib-name>"
    echo "Available: $(ls "${REPO_ROOT}/libraries/" | sed 's/\.yaml//' | tr '\n' ' ')"
    exit 1
fi

LIB_YAML="${REPO_ROOT}/libraries/${LIB_NAME}.yaml"
if [[ ! -f "${LIB_YAML}" ]]; then
    echo "ERROR: No config for '${LIB_NAME}' at ${LIB_YAML}"
    exit 1
fi

yaml_get() { grep "^${1}:" "${LIB_YAML}" | sed 's/^[^:]*:[[:space:]]*//' | tr -d '"'; }

write_pc() {
    local dir="$1" pc_name="$2" display_name="$3" ver="$4" libs="$5" cflags="$6"
    mkdir -p "${dir}/lib/pkgconfig"
    cat > "${dir}/lib/pkgconfig/${pc_name}.pc" << EOF
prefix=${dir}
exec_prefix=\${prefix}
libdir=\${exec_prefix}/lib
includedir=\${prefix}/include

Name: ${display_name}
Version: ${ver}
Description: wasm32-wasi cross-compiled build
Libs: -L\${libdir} ${libs}
Cflags: ${cflags}
EOF
}

LIB_VERSION="$(yaml_get version)"
SOURCE_URL="$(yaml_get source_url)"
SOURCE_SHA256="$(yaml_get source_sha256)"
SOURCE_FORMAT="$(yaml_get source_format)"
SOURCE_DIR_NAME="$(yaml_get source_dir)"
OUTPUT_NAME="$(yaml_get output_dir)"
DEPENDS="$(yaml_get depends 2>/dev/null || true)"

DEPS_DIR="${REPO_ROOT}/deps/${OUTPUT_NAME}"
CACHE_DIR="${HOME}/.cache/php-wasm/libs"
BUILD_TMP="${TMPDIR:-/tmp}/php-wasm-lib-${LIB_NAME}-${LIB_VERSION}"
PATCH_DIR="${REPO_ROOT}/patches/libs/${LIB_NAME}"
JOBS="${JOBS:-$(nproc 2>/dev/null || echo 4)}"

# shellcheck source=scripts/lib/wasi-env.sh
source "${SCRIPT_DIR}/lib/wasi-env.sh"

echo "==> build-lib: ${LIB_NAME} ${LIB_VERSION} → ${DEPS_DIR}"

if [[ -n "${DEPENDS}" ]]; then
    DEP_OUTPUT="$(grep "^output_dir:" "${REPO_ROOT}/libraries/${DEPENDS}.yaml" \
                  | sed 's/^[^:]*:[[:space:]]*//' | tr -d '"')"
    if [[ ! -d "${REPO_ROOT}/deps/${DEP_OUTPUT}/lib" ]]; then
        echo "==> Building dependency: ${DEPENDS}"
        "${SCRIPT_DIR}/build-lib.sh" "${DEPENDS}"
    fi
fi

mkdir -p "${CACHE_DIR}" "${BUILD_TMP}" "${DEPS_DIR}"
DEPS_DIR="$(cd "${DEPS_DIR}" && pwd)"

TARBALL="${CACHE_DIR}/${LIB_NAME}-${LIB_VERSION}.${SOURCE_FORMAT}"

if [[ ! -f "${TARBALL}" ]]; then
    echo "==> Downloading ${LIB_NAME} ${LIB_VERSION}..."
    curl -fsSL -o "${TARBALL}" "${SOURCE_URL}"
fi

if [[ -n "${SOURCE_SHA256}" ]]; then
    echo "${SOURCE_SHA256}  ${TARBALL}" | sha256sum -c --quiet || {
        echo "ERROR: sha256 mismatch for ${TARBALL}"
        rm -f "${TARBALL}"
        exit 1
    }
fi

SRC_DIR="${BUILD_TMP}/${SOURCE_DIR_NAME}"
if [[ ! -d "${SRC_DIR}" ]]; then
    echo "==> Extracting..."
    case "${SOURCE_FORMAT}" in
        zip)    unzip -q "${TARBALL}" -d "${BUILD_TMP}" ;;
        tar.gz) tar -xzf "${TARBALL}" -C "${BUILD_TMP}" ;;
        tar.xz) tar -xJf "${TARBALL}" -C "${BUILD_TMP}" ;;
        *) echo "ERROR: Unknown format: ${SOURCE_FORMAT}"; exit 1 ;;
    esac
fi

if [[ -d "${PATCH_DIR}" ]]; then
    PATCH_MARKER="${SRC_DIR}/.php-wasm-lib-patches-applied"
    if [[ ! -f "${PATCH_MARKER}" ]]; then
        for patch_file in $(ls "${PATCH_DIR}"/*.patch 2>/dev/null | sort); do
            echo "  Applying: $(basename "${patch_file}")"
            patch --strip=1 --forward --fuzz=3 --no-backup-if-mismatch \
                  --directory="${SRC_DIR}" < "${patch_file}" || \
                find "${SRC_DIR}" -name "*.rej" -delete 2>/dev/null || true
        done
        touch "${PATCH_MARKER}"
    fi
fi

mkdir -p "${DEPS_DIR}/include" "${DEPS_DIR}/lib"
cd "${SRC_DIR}"

case "${LIB_NAME}" in

sqlite3)
    "${CC}" \
        --sysroot="${WASI_SYSROOT}" --target=wasm32-wasi -O2 \
        -DSQLITE_OMIT_WAL=1 -DSQLITE_THREADSAFE=0 -DSQLITE_OMIT_LOAD_EXTENSION=1 \
        -c sqlite3.c -o "${BUILD_TMP}/sqlite3.o"
    "${AR}" rcs "${DEPS_DIR}/lib/libsqlite3.a" "${BUILD_TMP}/sqlite3.o"
    cp sqlite3.h "${DEPS_DIR}/include/sqlite3.h"
    write_pc "${DEPS_DIR}" "sqlite3" "SQLite" "${LIB_VERSION}" "-lsqlite3" "-I\${includedir}"
    ;;

zlib)
    OBJ_DIR="${BUILD_TMP}/zlib-objs"
    mkdir -p "${OBJ_DIR}"
    OBJS=()
    for src in adler32.c compress.c crc32.c deflate.c infback.c inffast.c \
               inflate.c inftrees.c trees.c uncompr.c zutil.c; do
        "${CC}" --sysroot="${WASI_SYSROOT}" --target=wasm32-wasi -O2 -DHAVE_HIDDEN \
                -c "${src}" -o "${OBJ_DIR}/${src%.c}.o"
        OBJS+=("${OBJ_DIR}/${src%.c}.o")
    done
    "${AR}" rcs "${DEPS_DIR}/lib/libz.a" "${OBJS[@]}"
    cp zlib.h zconf.h "${DEPS_DIR}/include/"
    write_pc "${DEPS_DIR}" "zlib" "zlib" "${LIB_VERSION}" "-lz" "-I\${includedir}"
    ;;

libxml2)
    HOST_TRIPLE="$(./config.guess 2>/dev/null || echo x86_64-pc-linux-gnu)"
    export CC AR RANLIB
    export CFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasi -O2 -D_WASI_EMULATED_SIGNAL"
    export LDFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasi -lwasi-emulated-signal"
    ./configure \
        --prefix="${DEPS_DIR}" --host=wasm32-wasi --build="${HOST_TRIPLE}" \
        --disable-shared --enable-static \
        --without-python --without-threads --without-http \
        --without-zlib --without-lzma --without-iconv \
        --without-modules --without-debug --without-catalog \
        --without-docbook --without-ftp --without-legacy
    make -j"${JOBS}" libxml2.la
    make install-libLTLIBRARIES install-data
    [[ -f ".libs/libxml2.a" && ! -f "${DEPS_DIR}/lib/libxml2.a" ]] && \
        cp .libs/libxml2.a "${DEPS_DIR}/lib/libxml2.a"
    write_pc "${DEPS_DIR}" "libxml-2.0" "libXML" "${LIB_VERSION}" \
             "-lxml2" "-I\${includedir}/libxml2"
    mkdir -p "${DEPS_DIR}/bin"
    # PHP's --with-libxml uses xml2-config to find headers and libs
    cat > "${DEPS_DIR}/bin/xml2-config" << XMLSH
#!/bin/sh
prefix=${DEPS_DIR}
libdir=\${prefix}/lib
includedir=\${prefix}/include
case "\$1" in
    --version) echo "${LIB_VERSION}" ;;
    --libs)    echo "-L\${libdir} -lxml2" ;;
    --cflags)  echo "-I\${includedir}/libxml2" ;;
    --prefix)  echo "\${prefix}" ;;
    *) echo "Usage: xml2-config [--version|--libs|--cflags|--prefix]"; exit 1 ;;
esac
XMLSH
    chmod +x "${DEPS_DIR}/bin/xml2-config"
    ;;

libpng)
    ZLIB_DIR="${REPO_ROOT}/deps/zlib-wasm"
    HOST_TRIPLE="$(./config.guess 2>/dev/null || echo x86_64-pc-linux-gnu)"
    export CC AR RANLIB
    export CFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasi -O2 \
        -I${ZLIB_DIR}/include -I${REPO_ROOT}/stubs"
    export LDFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasi -L${ZLIB_DIR}/lib"
    export LIBS="-lz"
    export PKG_CONFIG_PATH="${ZLIB_DIR}/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
    cp "${ZLIB_DIR}/include/zlib.h" zlib.h
    cp "${ZLIB_DIR}/include/zconf.h" zconf.h
    ./configure \
        --prefix="${DEPS_DIR}" --host=wasm32-wasi --build="${HOST_TRIPLE}" \
        --disable-shared --enable-static --disable-tools --without-binconfigs
    make -j"${JOBS}" libpng16.la
    make install-libLTLIBRARIES install-data
    [[ -f ".libs/libpng16.a" && ! -f "${DEPS_DIR}/lib/libpng16.a" ]] && \
        cp .libs/libpng16.a "${DEPS_DIR}/lib/libpng16.a"
    [[ -f "${DEPS_DIR}/lib/libpng16.a" && ! -e "${DEPS_DIR}/lib/libpng.a" ]] && \
        ln -s libpng16.a "${DEPS_DIR}/lib/libpng.a"
    write_pc "${DEPS_DIR}" "libpng16" "libpng" "${LIB_VERSION}" \
             "-lpng16" "-I\${includedir}/libpng16"
    cp "${DEPS_DIR}/lib/pkgconfig/libpng16.pc" "${DEPS_DIR}/lib/pkgconfig/libpng.pc"
    ;;

freetype)
    HOST_TRIPLE="$(./builds/unix/config.guess 2>/dev/null || echo x86_64-pc-linux-gnu)"
    export CC AR RANLIB
    export CFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasi -O2 \
        -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_GETPID \
        -mno-exception-handling -I${REPO_ROOT}/stubs"
    export LDFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasi \
        -lwasi-emulated-signal -lwasi-emulated-getpid"
    ./configure \
        --prefix="${DEPS_DIR}" --host=wasm32-wasi --build="${HOST_TRIPLE}" \
        --disable-shared --enable-static \
        --without-zlib --without-bzip2 --without-png \
        --without-harfbuzz --without-brotli
    make -j"${JOBS}"
    make install
    [[ ! -f "${DEPS_DIR}/lib/libfreetype.a" && -f "objs/.libs/libfreetype.a" ]] && \
        cp objs/.libs/libfreetype.a "${DEPS_DIR}/lib/libfreetype.a"
    write_pc "${DEPS_DIR}" "freetype2" "FreeType 2" "${LIB_VERSION}" \
             "-lfreetype" "-I\${includedir}/freetype2"
    ;;

libjpeg-turbo)
    if ! command -v cmake >/dev/null 2>&1; then
        echo "ERROR: cmake is required for libjpeg-turbo (apt install cmake)"; exit 1
    fi
    TOOLCHAIN_FILE="${WASI_SDK}/share/cmake/wasi-sdk.cmake"
    if [[ ! -f "${TOOLCHAIN_FILE}" ]]; then
        echo "ERROR: wasi-sdk CMake toolchain not found: ${TOOLCHAIN_FILE}"; exit 1
    fi
    CMAKE_BUILD="${BUILD_TMP}/cmake-build"
    mkdir -p "${CMAKE_BUILD}"
    cmake -S "${SRC_DIR}" -B "${CMAKE_BUILD}" \
        -DCMAKE_TOOLCHAIN_FILE="${TOOLCHAIN_FILE}" \
        -DCMAKE_INSTALL_PREFIX="${DEPS_DIR}" \
        -DENABLE_SHARED=OFF -DENABLE_STATIC=ON \
        -DWITH_TURBOJPEG=OFF -DWITH_JPEG8=ON
    cmake --build "${CMAKE_BUILD}" -j"${JOBS}"
    cmake --install "${CMAKE_BUILD}"
    ;;

*)
    echo "ERROR: No build logic for '${LIB_NAME}'. Add a case block to build-lib.sh."
    exit 1
    ;;
esac

echo "==> Done: ${LIB_NAME} ${LIB_VERSION}"
