#!/usr/bin/env bash
# Build the pinned native libraries used by the Moodle PHP profile for WASI.
# Optional arguments select libraries (their prerequisites must already exist).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/lib/wasi-env.sh"
PREFIX="${ROOT}/deps/moodle-wasm"
CACHE="${HOME}/.cache/php-wasm/moodle-libs"
WORK="${TMPDIR:-/tmp}/php-wasm-moodle-deps"
JOBS="${JOBS:-8}"
mkdir -p "${PREFIX}" "${CACHE}" "${WORK}"
export CC CXX AR RANLIB NM
export CFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasip1 -O2 -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_GETPID -D_WASI_EMULATED_PROCESS_CLOCKS -I${PREFIX}/include -I${ROOT}/stubs"
export CXXFLAGS="${CFLAGS} -fno-exceptions"
export LDFLAGS="--sysroot=${WASI_SYSROOT} --target=wasm32-wasip1 -L${PREFIX}/lib -lwasi-emulated-signal -lwasi-emulated-getpid -lwasi-emulated-process-clocks"
export PKG_CONFIG_LIBDIR="${PREFIX}/lib/pkgconfig"
export PKG_CONFIG_PATH="${PKG_CONFIG_LIBDIR}"
libs=("$@")
[[ ${#libs[@]} -gt 0 ]] || libs=(iconv zlib png zip sodium openssl icu curl)
for lib in "${libs[@]}"; do
    mapfile -t input < <(python3 - "${ROOT}/libraries/moodle.lock.json" "${lib}" <<'PY'
import json, sys
item = json.load(open(sys.argv[1]))[sys.argv[2]]
for field in ('url', 'sha256', 'archive'):
    print(item[field])
PY
    )
    archive="${CACHE}/${input[2]}"
    if [[ ! -f "${archive}" ]]; then
        curl -fsSL --retry 3 "${input[0]}" -o "${archive}.partial"
        mv "${archive}.partial" "${archive}"
    fi
    printf '%s  %s\n' "${input[1]}" "${archive}" | sha256sum --check --status
    src="${WORK}/${lib}"
    if [[ ! -d "${src}" ]]; then
        mkdir -p "${src}"
        tar -xf "${archive}" --strip-components=1 -C "${src}"
    fi
    echo "Building ${lib} for WASI"
    cd "${src}"
    cmake_args=(-DCMAKE_TOOLCHAIN_FILE="${WASI_SDK}/share/cmake/wasi-sdk-p1.cmake"
        -DCMAKE_INSTALL_PREFIX="${PREFIX}" -DCMAKE_PREFIX_PATH="${PREFIX}"
        -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF
        -DCMAKE_C_FLAGS="${CFLAGS}" -DCMAKE_CXX_FLAGS="${CXXFLAGS}")
    case "${lib}" in
        iconv)
            ./configure --host=wasm32-wasi --build="$(gcc -dumpmachine)" --prefix="${PREFIX}" --disable-shared --enable-static --disable-nls
            make -j"${JOBS}" -C libcharset
            make -C libcharset install-lib
            make -j"${JOBS}" -C lib
            make -C lib install
            install -Dm644 include/iconv.h.inst "${PREFIX}/include/iconv.h"
            ;;
        zlib)
            cmake -S . -B build "${cmake_args[@]}" -DZLIB_BUILD_TESTING=OFF -DZLIB_BUILD_SHARED=OFF
            cmake --build build -j"${JOBS}"
            cmake --install build
            ;;
        png)
            cmake -S . -B build "${cmake_args[@]}" -DPNG_SHARED=OFF -DPNG_STATIC=ON -DPNG_TESTS=OFF -DPNG_TOOLS=OFF -DPNG_HARDWARE_OPTIMIZATIONS=OFF
            cmake --build build -j"${JOBS}"
            cmake --install build
            # libpng's WASI CMake platform uses the fallback target name.
            if [[ -f "${PREFIX}/lib/liblibpng16_static.a" ]]; then
                cp "${PREFIX}/lib/liblibpng16_static.a" "${PREFIX}/lib/libpng16.a"
            fi
            ;;
        zip)
            cmake -S . -B build "${cmake_args[@]}" -DENABLE_COMMONCRYPTO=OFF -DENABLE_GNUTLS=OFF -DENABLE_MBEDTLS=OFF -DENABLE_OPENSSL=OFF -DENABLE_BZIP2=OFF -DENABLE_LZMA=OFF -DENABLE_ZSTD=OFF -DBUILD_TOOLS=OFF -DBUILD_REGRESS=OFF -DBUILD_EXAMPLES=OFF -DBUILD_DOC=OFF
            cmake --build build -j"${JOBS}"
            cmake --install build
            ;;
        sodium)
            ./configure --host=wasm32-wasi --build="$(gcc -dumpmachine)" --prefix="${PREFIX}" --disable-shared --enable-static --disable-asm --disable-pie
            make -j"${JOBS}"
            make install
            ;;
        openssl)
            ./Configure gcc --prefix="${PREFIX}" --libdir=lib no-shared no-asm no-threads no-dso no-engine no-sock no-ui-console no-tests no-secure-memory
            make -j"${JOBS}" build_libs
            make install_dev
            ;;
        icu)
            # Static archive rules are portable; ICU has no WASI platform file.
            cp source/config/mh-linux source/config/mh-unknown
            mkdir -p "${WORK}/icu-native"
            (cd "${WORK}/icu-native"; env -u CFLAGS -u CXXFLAGS -u LDFLAGS -u AR -u RANLIB -u NM CC=gcc CXX=g++ "${src}/source/runConfigureICU" Linux --disable-tests --disable-samples --disable-shared --enable-static; make -j"${JOBS}")
            mkdir -p build
            (cd build; CFLAGS="${CFLAGS} -DU_HAVE_MMAP=0 -DU_HAVE_THREADS=0 -DU_HAVE_TZNAME=0" CXXFLAGS="${CXXFLAGS} -DU_HAVE_MMAP=0 -DU_HAVE_THREADS=0 -DU_HAVE_TZNAME=0" ../source/configure --host=wasm32-wasi --build="$(gcc -dumpmachine)" --prefix="${PREFIX}" --with-cross-build="${WORK}/icu-native" --disable-tests --disable-samples --disable-extras --disable-tools --disable-shared --enable-static --disable-dyload --with-data-packaging=static; make -j"${JOBS}" PKGDATA_OPTS="-w -O ../data/icupkg.inc"; make install PKGDATA_OPTS="-w -O ../data/icupkg.inc")
            ;;
        curl)
            "${CC}" ${CFLAGS} -I"${ROOT}/runtime/wasi-offline/include" -c "${ROOT}/runtime/wasi-offline/network.c" -o "${WORK}/offline-network.o"
            "${AR}" rcs "${PREFIX}/lib/libwasi-offline-network.a" "${WORK}/offline-network.o"
            export CFLAGS="${CFLAGS} -I${ROOT}/runtime/wasi-offline/include"
            export LIBS="-lwasi-offline-network"
            ./configure --host=wasm32-wasi --build="$(gcc -dumpmachine)" --prefix="${PREFIX}" --disable-shared --enable-static --disable-threaded-resolver --disable-unix-sockets --disable-socketpair --disable-ipv6 --without-ssl --without-libpsl --without-libidn2 --without-librtmp --without-zstd --without-brotli --disable-ldap --disable-ldaps --disable-rtsp --disable-dict --disable-telnet --disable-tftp --disable-pop3 --disable-imap --disable-smtp --disable-gopher --disable-mqtt --disable-ftp --disable-smb --disable-manual
            make -j"${JOBS}" -C lib CFLAGS="${CFLAGS} -include offline-network.h -DHAVE_SOCKET=1"
            make -C lib install
            make -C include install
            install -Dm644 libcurl.pc "${PREFIX}/lib/pkgconfig/libcurl.pc"
            ;;
        *) echo "Unknown library: ${lib}" >&2; exit 2 ;;
    esac
done
