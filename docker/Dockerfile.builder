# syntax=docker/dockerfile:1.4
# Multi-stage build for PHP WASM/WASI compilation.
#
# Stages:
#   base      — Ubuntu 24.04 + build dependencies
#   wasi-sdk  — adds wasi-sdk 24 and wasm-opt (binaryen)
#   build     — copies repo, fetches PHP source, patches, builds .wasm

ARG WASI_SDK_VERSION=24
ARG WASI_SDK_SHA256=c6c38aab56e5de88adf6c1ebc9c3ae8da72f88ec2b656fb024eda8d4167a0bc5
ARG BINARYEN_VERSION=117

# ============================================================
# Stage 1: base
# ============================================================
FROM ubuntu:24.04 AS base

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    # Autotools / build system
    autoconf \
    automake \
    libtool \
    make \
    cmake \
    ninja-build \
    pkg-config \
    bison \
    re2c \
    flex \
    # Host compiler (required for buildconf + cross-compile host tools)
    gcc \
    g++ \
    binutils \
    # PHP native build deps (headers used even in cross builds via pkg-config)
    libxml2-dev \
    libsqlite3-dev \
    libonig-dev \
    libzip-dev \
    libssl-dev \
    zlib1g-dev \
    libjpeg-dev \
    libpng-dev \
    libfreetype6-dev \
    libgmp-dev \
    libreadline-dev \
    libicu-dev \
    # Download + network tools
    curl \
    wget \
    ca-certificates \
    # Misc utilities
    git \
    xz-utils \
    jq \
    python3 \
    ccache \
    # WASM tools (host-side, for validation)
    wabt \
    && rm -rf /var/lib/apt/lists/*

# ============================================================
# Stage 2: wasi-sdk
# ============================================================
FROM base AS wasi-sdk

ARG WASI_SDK_VERSION
ARG WASI_SDK_SHA256
ARG BINARYEN_VERSION

ENV WASI_SDK_PATH=/opt/wasi-sdk

# Install wasi-sdk
RUN WASI_SDK_FULL="${WASI_SDK_VERSION}.0" && \
    WASI_SDK_URL="https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-${WASI_SDK_VERSION}/wasi-sdk-${WASI_SDK_FULL}-x86_64-linux.tar.gz" && \
    echo "Downloading wasi-sdk ${WASI_SDK_FULL} from ${WASI_SDK_URL}" && \
    curl -fsSL --retry 3 -o /tmp/wasi-sdk.tar.gz "${WASI_SDK_URL}" && \
    echo "${WASI_SDK_SHA256}  /tmp/wasi-sdk.tar.gz" | sha256sum -c - && \
    mkdir -p "${WASI_SDK_PATH}" && \
    tar -xzf /tmp/wasi-sdk.tar.gz -C "${WASI_SDK_PATH}" --strip-components=1 && \
    rm /tmp/wasi-sdk.tar.gz && \
    echo "wasi-sdk installed:" && \
    "${WASI_SDK_PATH}/bin/clang" --version

# Install wasm-opt (binaryen)
RUN curl -fsSL --retry 3 -o /tmp/binaryen.tar.gz \
    "https://github.com/WebAssembly/binaryen/releases/download/version_${BINARYEN_VERSION}/binaryen-version_${BINARYEN_VERSION}-x86_64-linux.tar.gz" && \
    tar -xzf /tmp/binaryen.tar.gz -C /usr/local --strip-components=1 && \
    rm /tmp/binaryen.tar.gz && \
    echo "wasm-opt version: $(wasm-opt --version)"

# Configure ccache to wrap wasi-sdk clang
RUN mkdir -p /usr/lib/ccache && \
    for tool in clang clang++ llvm-ar llvm-ranlib; do \
        ln -sf /usr/bin/ccache "/usr/lib/ccache/${tool}"; \
    done

# ============================================================
# Stage 3: build
# ============================================================
FROM wasi-sdk AS build

# WASI compilation environment
ENV WASI_SDK_PATH=/opt/wasi-sdk
ENV WASI_SYSROOT=/opt/wasi-sdk/share/wasi-sysroot
ENV TARGET=wasm32-wasi

ENV CC="${WASI_SDK_PATH}/bin/clang"
ENV CXX="${WASI_SDK_PATH}/bin/clang++"
ENV AR="${WASI_SDK_PATH}/bin/llvm-ar"
ENV RANLIB="${WASI_SDK_PATH}/bin/llvm-ranlib"
ENV NM="${WASI_SDK_PATH}/bin/llvm-nm"
ENV STRIP="${WASI_SDK_PATH}/bin/llvm-strip"
ENV LD="${WASI_SDK_PATH}/bin/wasm-ld"

# CFLAGS match configure-php.sh — validated against VMware WasmLabs wlr-build.sh.
# Key differences vs naive WASI port:
#   - NO -D_WASI_EMULATED_MMAN: Zend allocator falls back to malloc (HAVE_MMAP=0)
#   - NO -fstack-protector-strong: wasi-libc has no __stack_chk_fail
#   - NO -fno-exceptions: PHP uses setjmp/longjmp, not C++ EH
#   - ADD -DHAVE_FORK=0: disables fork() code paths
#   - ADD -D_POSIX_SOURCE=1 -D_GNU_SOURCE=1: expose POSIX/GNU extensions
#   - ADD -DWASM_WASI=1: activates php-wasm-specific WASI patches
ENV CFLAGS="--sysroot=${WASI_SYSROOT} -O2 -D_WASI_EMULATED_GETPID -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -D_POSIX_SOURCE=1 -D_GNU_SOURCE=1 -DHAVE_FORK=0 -DWASM_WASI=1"
ENV LDFLAGS="--sysroot=${WASI_SYSROOT} -lwasi-emulated-getpid -lwasi-emulated-signal -lwasi-emulated-process-clocks -Wl,--stack-first -Wl,-z,stack-size=2097152"

ENV CCACHE_DIR=/ccache
ENV PATH="/usr/lib/ccache:${WASI_SDK_PATH}/bin:${PATH}"

WORKDIR /build

# Copy repo files
COPY patches/  /build/patches/
COPY versions/ /build/versions/
COPY profiles/ /build/profiles/
COPY scripts/  /build/scripts/
COPY docker/   /build/docker/

RUN chmod +x /build/scripts/*.sh /build/docker/entrypoint.sh

# Build arguments
ARG PHP_VERSION=8.3
ARG PROFILE=default
ARG OUTPUT_DIR=/out
ARG RUN_WASM_OPT=0

ENV PHP_VERSION=${PHP_VERSION}
ENV PROFILE=${PROFILE}
ENV OUTPUT_DIR=${OUTPUT_DIR}
ENV RUN_WASM_OPT=${RUN_WASM_OPT}

# Pre-fetch source during build (cached by BuildKit)
RUN --mount=type=cache,target=/ccache \
    --mount=type=cache,target=/tmp/php-src-cache \
    /build/scripts/fetch-source.sh "${PHP_VERSION}" "/tmp/php-src-cache"

ENTRYPOINT ["/build/docker/entrypoint.sh"]
CMD []
