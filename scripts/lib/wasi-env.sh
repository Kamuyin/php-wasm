#!/usr/bin/env bash
# Source this file to set up the wasi-sdk toolchain environment.
# Exports: WASI_SDK, WASI_SYSROOT, CC, CXX, AR, RANLIB, NM, STRIP
# Caller is responsible for setting CFLAGS/LDFLAGS as needed.

WASI_SDK="${WASI_SDK_PATH:-/opt/wasi-sdk}"
if [[ ! -d "${WASI_SDK}" ]]; then
    echo "ERROR: wasi-sdk not found at '${WASI_SDK}'. Set WASI_SDK_PATH." >&2
    exit 1
fi

WASI_SYSROOT="${WASI_SDK}/share/wasi-sysroot"
if [[ ! -d "${WASI_SYSROOT}" ]]; then
    echo "ERROR: wasi-sysroot not found: ${WASI_SYSROOT}" >&2
    exit 1
fi

CC="${WASI_SDK}/bin/clang"
CXX="${WASI_SDK}/bin/clang++"
AR="${WASI_SDK}/bin/llvm-ar"
RANLIB="${WASI_SDK}/bin/llvm-ranlib"
NM="${WASI_SDK}/bin/llvm-nm"
STRIP="${WASI_SDK}/bin/llvm-strip"

for _tool in "${CC}" "${AR}" "${RANLIB}" "${NM}"; do
    if [[ ! -x "${_tool}" ]]; then
        echo "ERROR: Required tool not found: ${_tool}" >&2
        exit 1
    fi
done
unset _tool

export WASI_SDK WASI_SYSROOT CC CXX AR RANLIB NM STRIP
