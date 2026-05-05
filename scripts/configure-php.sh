#!/usr/bin/env bash
# Usage: configure-php.sh <version> <profile> <php-src-dir>
# Runs buildconf --force, then builds and executes the ./configure invocation
# from config.yaml + profile.yaml using the wasi-sdk toolchain.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

VERSION="${1:-}"
PROFILE="${2:-}"
PHP_SRC="${3:-}"

if [[ -z "${VERSION}" || -z "${PROFILE}" || -z "${PHP_SRC}" ]]; then
    echo "Usage: $0 <version> <profile> <php-src-dir>"
    exit 1
fi

# Locate wasi-sdk
WASI_SDK="${WASI_SDK_PATH:-/opt/wasi-sdk}"
if [[ ! -d "${WASI_SDK}" ]]; then
    echo "ERROR: wasi-sdk not found at '${WASI_SDK}'"
    echo "  Install wasi-sdk 24+ from https://github.com/WebAssembly/wasi-sdk/releases"
    echo "  Or set WASI_SDK_PATH=/path/to/wasi-sdk"
    exit 1
fi

WASI_SYSROOT="${WASI_SDK}/share/wasi-sysroot"
if [[ ! -d "${WASI_SYSROOT}" ]]; then
    echo "ERROR: wasi-sysroot not found: ${WASI_SYSROOT}"
    exit 1
fi

CC="${WASI_SDK}/bin/clang"
CXX="${WASI_SDK}/bin/clang++"
AR="${WASI_SDK}/bin/llvm-ar"
RANLIB="${WASI_SDK}/bin/llvm-ranlib"
NM="${WASI_SDK}/bin/llvm-nm"
STRIP="${WASI_SDK}/bin/llvm-strip"

for tool in "${CC}" "${CXX}" "${AR}" "${RANLIB}" "${NM}"; do
    if [[ ! -x "${tool}" ]]; then
        echo "ERROR: Required tool not found: ${tool}"
        echo "  wasi-sdk installation may be incomplete"
        exit 1
    fi
done

TARGET="wasm32-wasi"
# host_alias=wasm32-musl-wasi is used by VMware WLR; it makes autoconf pick
# the right default libc without triggering glibc-specific configure paths.
HOST_ALIAS="wasm32-musl-wasi"

# Detect build (host) triple for --build configure flag
BUILD_TRIPLE=$(gcc -dumpmachine 2>/dev/null || echo "x86_64-linux-gnu")

# ─── CFLAGS ──────────────────────────────────────────────────────────────────
# Based on VMware WasmLabs' validated wlr-build.sh configuration.
#
# -D_WASI_EMULATED_GETPID       : getpid() via wasi-libc emulation layer
# -D_WASI_EMULATED_SIGNAL       : signal() via wasi-libc emulation layer
# -D_WASI_EMULATED_PROCESS_CLOCKS: clock_gettime(CLOCK_PROCESS_CPUTIME_ID)
# -D_POSIX_SOURCE=1 -D_GNU_SOURCE=1 : expose POSIX / GNU extensions in headers
# -DHAVE_FORK=0                 : disables fork() code paths in PHP
# -DWASM_WASI=1                 : activates php-wasm WASI patches
#
# Intentionally excluded vs. a naive WASI port attempt:
# -D_WASI_EMULATED_MMAN  : not needed; Zend allocator falls back to malloc when
#                           HAVE_MMAP=0 (set by configure for wasi-libc sysroot)
# -fstack-protector-strong: wasi-libc has no __stack_chk_fail; link errors
# -fno-exceptions         : PHP uses setjmp/longjmp, not C++ exceptions
CFLAGS="--sysroot=${WASI_SYSROOT}"
CFLAGS="${CFLAGS} -O2"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_GETPID"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_SIGNAL"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_PROCESS_CLOCKS"
CFLAGS="${CFLAGS} -D_POSIX_SOURCE=1"
CFLAGS="${CFLAGS} -D_GNU_SOURCE=1"
CFLAGS="${CFLAGS} -DHAVE_FORK=0"
CFLAGS="${CFLAGS} -DWASM_WASI=1"
CFLAGS="${CFLAGS} -DPHP_WASM_PROFILE_NAME=\"${PROFILE}\""

# ─── LDFLAGS ─────────────────────────────────────────────────────────────────
# autoconf runs compile+link tests; LDFLAGS must carry the sysroot too.
# Note: wasi-sdk's clang driver accepts --sysroot on both compile and link steps.
LDFLAGS="--sysroot=${WASI_SYSROOT}"
LDFLAGS="${LDFLAGS} -lwasi-emulated-getpid"
LDFLAGS="${LDFLAGS} -lwasi-emulated-signal"
LDFLAGS="${LDFLAGS} -lwasi-emulated-process-clocks"
LDFLAGS="${LDFLAGS} -Wl,--stack-first"
LDFLAGS="${LDFLAGS} -Wl,-z,stack-size=2097152"

# autoconf compiles + links during feature detection; passing LDFLAGS inside
# CFLAGS ensures link tests also pick up the WASI emulation libs.
CFLAGS="${CFLAGS} ${LDFLAGS}"

# Export for use by PHP's build system
export PHP_WASM_PROFILE="${PROFILE}"

# Prepend our cross-compiled deps pkgconfig dir so PHP's PKG_CHECK_MODULES
# finds our WASM-compiled libraries instead of the host system ones.
WASM_PKGCONFIG=""
for pc_dir in "${REPO_ROOT}"/deps/*/lib/pkgconfig; do
    [[ -d "${pc_dir}" ]] || continue
    WASM_PKGCONFIG="${WASM_PKGCONFIG:+${WASM_PKGCONFIG}:}${pc_dir}"
done
if [[ -n "${WASM_PKGCONFIG}" ]]; then
    export PKG_CONFIG_PATH="${WASM_PKGCONFIG}:${PKG_CONFIG_PATH:-}"
fi

# Read a YAML list block without requiring yq
read_yaml_list() {
    local file="$1"
    local key="$2"
    local in_list=0
    while IFS= read -r line; do
        if [[ "${line}" =~ ^${key}: ]]; then
            in_list=1; continue
        fi
        if [[ "${in_list}" -eq 1 ]]; then
            if [[ "${line}" =~ ^[[:space:]]+-[[:space:]]\"?([^\"]+)\"?[[:space:]]*$ ]]; then
                echo "${BASH_REMATCH[1]}"
            elif [[ ! "${line}" =~ ^[[:space:]]+-[[:space:]] && "${line}" =~ [^[:space:]] ]]; then
                in_list=0
            fi
        fi
    done < "${file}"
}

CONFIG_YAML="${REPO_ROOT}/versions/${VERSION}/config.yaml"
PROFILE_YAML="${REPO_ROOT}/profiles/${PROFILE}.yaml"

mapfile -t BASE_FLAGS    < <(read_yaml_list "${CONFIG_YAML}"  "configure_extra")
mapfile -t PROFILE_FLAGS < <(read_yaml_list "${PROFILE_YAML}" "configure_flags")

# Build configure invocation
CONFIGURE_ARGS=(
    "--host=${TARGET}"
    "--build=${BUILD_TRIPLE}"
    "host_alias=${HOST_ALIAS}"
    "--target=${TARGET}"
    "target_alias=${HOST_ALIAS}"
    "--prefix=/usr"
    "--sysconfdir=/etc/php"
    "--with-config-file-path=/etc/php"
    "--with-config-file-scan-dir=/etc/php/conf.d"
    "CC=${CC}"
    "CXX=${CXX}"
    "AR=${AR}"
    "RANLIB=${RANLIB}"
    "NM=${NM}"
    "STRIP=${STRIP}"
    "CFLAGS=${CFLAGS}"
    "LDFLAGS=${LDFLAGS}"
)

for flag in "${BASE_FLAGS[@]+"${BASE_FLAGS[@]}"}"; do
    CONFIGURE_ARGS+=("${flag}")
done
for flag in "${PROFILE_FLAGS[@]+"${PROFILE_FLAGS[@]}"}"; do
    # Expand "deps/<path>" to an absolute path rooted at the repo.
    # This lets profiles reference pre-built WASM dependency libraries portably.
    flag="${flag/deps\//${REPO_ROOT}/deps/}"
    CONFIGURE_ARGS+=("${flag}")
done

echo "  WASI SDK   : ${WASI_SDK}"
echo "  Target     : ${TARGET} (alias: ${HOST_ALIAS})"
echo "  Sysroot    : ${WASI_SYSROOT}"
echo "  Profile    : ${PROFILE} (${#PROFILE_FLAGS[@]} flags)"
echo ""
echo "  Full configure command:"
printf '    ./configure \\\n'
printf '      %s \\\n' "${CONFIGURE_ARGS[@]}"
echo ""

# Always run buildconf --force. Patches modify configure.ac and generated
# files (e.g. ext/standard/basic_functions_arginfo.h via stub.php).
# buildconf regenerates configure + config.h.in from the patched sources.
echo "  Running buildconf --force..."
cd "${PHP_SRC}"
./buildconf --force 2>&1 | tail -10

echo "  Running configure..."
./configure "${CONFIGURE_ARGS[@]}" 2>&1 | tail -30
echo "  Configure complete"
