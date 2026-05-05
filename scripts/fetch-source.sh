#!/usr/bin/env bash
# Usage: fetch-source.sh <version> <build-dir>
# Downloads PHP source tarball and verifies sha256. Cache-aware.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

VERSION="${1:-}"
BUILD_DIR="${2:-}"

if [[ -z "${VERSION}" || -z "${BUILD_DIR}" ]]; then
    echo "Usage: $0 <version> <build-dir>"
    echo "  version    PHP minor version: 8.2, 8.3, 8.4"
    echo "  build-dir  Directory to store tarball and extracted source"
    exit 1
fi

SOURCE_YAML="${REPO_ROOT}/versions/${VERSION}/source.yaml"
if [[ ! -f "${SOURCE_YAML}" ]]; then
    echo "ERROR: source.yaml not found: ${SOURCE_YAML}"
    exit 1
fi

# Parse YAML fields without requiring yq
yaml_field() {
    grep "^${1}:" "${SOURCE_YAML}" | sed "s/${1}: *\"//" | sed 's/"//'
}

PHP_VERSION=$(yaml_field "php_version")
SOURCE_TAG=$(yaml_field "source_tag")
EXPECTED_SHA256=$(yaml_field "source_sha256")
DOWNLOAD_URL=$(yaml_field "download_url")
DOWNLOAD_URL_ALT=$(yaml_field "download_url_alt")

if [[ -z "${PHP_VERSION}" || -z "${SOURCE_TAG}" ]]; then
    echo "ERROR: Could not parse php_version or source_tag from ${SOURCE_YAML}"
    exit 1
fi

TARBALL="${BUILD_DIR}/${SOURCE_TAG}.tar.gz"
EXTRACT_DIR="${BUILD_DIR}/php-src-${SOURCE_TAG}"

# Cache hit: extracted source already present
if [[ -d "${EXTRACT_DIR}" ]]; then
    echo "  Cache hit: ${EXTRACT_DIR} already extracted"
    exit 0
fi

mkdir -p "${BUILD_DIR}"

# Download tarball if not already cached
if [[ -f "${TARBALL}" ]]; then
    echo "  Tarball already cached: ${TARBALL}"
else
    echo "  Downloading PHP ${PHP_VERSION}..."
    echo "  URL: ${DOWNLOAD_URL}"

    download_ok=0
    if command -v curl &>/dev/null; then
        if curl -fsSL --retry 3 --retry-delay 5 \
                --connect-timeout 30 \
                -o "${TARBALL}" "${DOWNLOAD_URL}"; then
            download_ok=1
        else
            echo "  Primary URL failed; trying alternate..."
            curl -fsSL --retry 3 --retry-delay 5 \
                --connect-timeout 30 \
                -o "${TARBALL}" "${DOWNLOAD_URL_ALT}" && download_ok=1
        fi
    elif command -v wget &>/dev/null; then
        if wget -q --tries=3 --timeout=30 \
                -O "${TARBALL}" "${DOWNLOAD_URL}"; then
            download_ok=1
        else
            echo "  Primary URL failed; trying alternate..."
            wget -q --tries=3 --timeout=30 \
                -O "${TARBALL}" "${DOWNLOAD_URL_ALT}" && download_ok=1
        fi
    else
        echo "ERROR: curl or wget required for downloading PHP source"
        exit 1
    fi

    if [[ "${download_ok}" -eq 0 ]]; then
        echo "ERROR: Download failed from both primary and alternate URLs"
        rm -f "${TARBALL}"
        exit 1
    fi

    echo "  Downloaded: ${TARBALL} ($(du -sh "${TARBALL}" | cut -f1))"
fi

# Verify sha256
if [[ "${EXPECTED_SHA256}" == "PLACEHOLDER_SHA256_REPLACE_BEFORE_BUILD" ]]; then
    echo ""
    echo "  WARNING: sha256 is a placeholder — skipping verification"
    echo "  This is NOT safe for production builds!"
    echo "  To fix: sha256sum ${TARBALL}"
    echo "         Then update source_sha256 in ${SOURCE_YAML}"
    echo ""
else
    echo "  Verifying sha256..."
    ACTUAL_SHA256=$(sha256sum "${TARBALL}" | awk '{print $1}')
    if [[ "${ACTUAL_SHA256}" != "${EXPECTED_SHA256}" ]]; then
        echo "ERROR: sha256 mismatch!"
        echo "  Expected : ${EXPECTED_SHA256}"
        echo "  Actual   : ${ACTUAL_SHA256}"
        echo "  File     : ${TARBALL}"
        rm -f "${TARBALL}"
        exit 1
    fi
    echo "  sha256 OK"
fi

# Extract into a canonical directory so callers do not depend on the upstream
# archive's top-level folder naming convention.
echo "  Extracting to ${EXTRACT_DIR}..."
rm -rf "${EXTRACT_DIR}"
mkdir -p "${EXTRACT_DIR}"
tar -xzf "${TARBALL}" -C "${EXTRACT_DIR}" --strip-components=1

if [[ ! -f "${EXTRACT_DIR}/configure.ac" ]]; then
    echo "ERROR: Extraction succeeded but ${EXTRACT_DIR} does not look like a PHP source tree"
    exit 1
fi

echo "  Source ready: ${EXTRACT_DIR}"
