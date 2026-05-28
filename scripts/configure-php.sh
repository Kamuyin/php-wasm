#!/usr/bin/env bash
# Usage: configure-php.sh <version> <profile> <php-src-dir>
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

# shellcheck source=scripts/lib/wasi-env.sh
source "${SCRIPT_DIR}/lib/wasi-env.sh"

TARGET="wasm32-wasi"
# wasm32-musl-wasi is the VMware WasmLabs alias; autoconf picks the right
# libc defaults with this instead of triggering glibc-specific configure paths.
HOST_ALIAS="wasm32-musl-wasi"
BUILD_TRIPLE=$(gcc -dumpmachine 2>/dev/null || echo "x86_64-linux-gnu")

# CFLAGS: see VMware WasmLabs wlr-build.sh for the full rationale.
# _WASI_EMULATED_MMAN is needed for OPcache's configure check + patch 0006
# redirects zend chunk allocation to aligned_alloc/free to avoid munmap failures.
CFLAGS="--sysroot=${WASI_SYSROOT} -O2"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_GETPID"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_SIGNAL"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_PROCESS_CLOCKS"
CFLAGS="${CFLAGS} -D_WASI_EMULATED_MMAN"
CFLAGS="${CFLAGS} -D_POSIX_SOURCE=1"
CFLAGS="${CFLAGS} -D_GNU_SOURCE=1"
CFLAGS="${CFLAGS} -DHAVE_FORK=0"
CFLAGS="${CFLAGS} -DWASM_WASI=1"
CFLAGS="${CFLAGS} -DPHP_WASM_PROFILE_NAME=\"${PROFILE}\""
CFLAGS="${CFLAGS} -mno-exception-handling"
CFLAGS="${CFLAGS} -I${REPO_ROOT}/stubs"

LDFLAGS="--sysroot=${WASI_SYSROOT}"
LDFLAGS="${LDFLAGS} -lwasi-emulated-getpid"
LDFLAGS="${LDFLAGS} -lwasi-emulated-signal"
LDFLAGS="${LDFLAGS} -lwasi-emulated-process-clocks"
LDFLAGS="${LDFLAGS} -lwasi-emulated-mman"
LDFLAGS="${LDFLAGS} -Wl,--stack-first"
LDFLAGS="${LDFLAGS} -Wl,-z,stack-size=2097152"

# autoconf link tests need LDFLAGS available during compile step too
CFLAGS="${CFLAGS} ${LDFLAGS}"

export PHP_WASM_PROFILE="${PROFILE}"

# Prepend WASM dep pkgconfig dirs so PHP's PKG_CHECK_MODULES finds our
# cross-compiled libraries instead of the host system ones.
WASM_PKGCONFIG=""
for pc_dir in "${REPO_ROOT}"/deps/*/lib/pkgconfig; do
    [[ -d "${pc_dir}" ]] || continue
    WASM_PKGCONFIG="${WASM_PKGCONFIG:+${WASM_PKGCONFIG}:}${pc_dir}"
done
[[ -n "${WASM_PKGCONFIG}" ]] && export PKG_CONFIG_PATH="${WASM_PKGCONFIG}:${PKG_CONFIG_PATH:-}"

read_yaml_list() {
    local file="$1" key="$2" in_list=0
    while IFS= read -r line; do
        if [[ "${line}" =~ ^${key}: ]]; then
            in_list=1; continue
        fi
        if [[ "${in_list}" -eq 1 ]]; then
            [[ "${line}" =~ ^[[:space:]]*# ]] && continue
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
    flag="${flag/deps\//${REPO_ROOT}/deps/}"

    # Pre-flight check: verify required dep directories exist before configure runs
    if [[ "${flag}" =~ ^--with-[^=]+=(.+/deps/([^/[:space:]]+)) ]]; then
        dep_path="${BASH_REMATCH[1]}"
        dep_name="${BASH_REMATCH[2]}"
        if [[ ! -d "${dep_path}" ]]; then
            echo ""
            echo "ERROR: Profile '${PROFILE}' requires dep '${dep_name}' but not found: ${dep_path}"
            echo "  Build it first:  scripts/build-lib.sh ${dep_name%.wasm}"
            echo ""
            exit 1
        fi
    fi

    CONFIGURE_ARGS+=("${flag}")
done

echo "  WASI SDK : ${WASI_SDK}"
echo "  Target   : ${TARGET} (alias: ${HOST_ALIAS})"
echo "  Profile  : ${PROFILE} (${#PROFILE_FLAGS[@]} flags)"
echo ""

# Patches modify configure.ac; always force regeneration.
echo "  Running buildconf --force..."
cd "${PHP_SRC}"
./buildconf --force 2>&1 | tail -10

echo "  Running configure..."
./configure "${CONFIGURE_ARGS[@]}" 2>&1 | tail -30
echo "  Configure complete"
