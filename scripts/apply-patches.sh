#!/usr/bin/env bash
# Usage: apply-patches.sh <version> <php-src-dir>
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

VERSION="${1:-}"
PHP_SRC="${2:-}"

if [[ -z "${VERSION}" || -z "${PHP_SRC}" ]]; then
    echo "Usage: $0 <version> <php-src-dir>"
    exit 1
fi

VERSION_DIR="${REPO_ROOT}/versions/${VERSION}"
CONFIG_YAML="${VERSION_DIR}/config.yaml"
COMMON_PATCHES_DIR="${REPO_ROOT}/patches/php"

if [[ ! -d "${PHP_SRC}" ]]; then
    echo "ERROR: PHP source directory not found: ${PHP_SRC}"
    exit 1
fi

if [[ ! -f "${CONFIG_YAML}" ]]; then
    echo "ERROR: config.yaml not found: ${CONFIG_YAML}"
    exit 1
fi

# Parse a YAML list block into lines (no yq dependency)
read_yaml_list() {
    local yaml_file="$1"
    local key="$2"
    local in_list=0
    while IFS= read -r line; do
        if [[ "${line}" =~ ^${key}: ]]; then
            in_list=1
            continue
        fi
        if [[ "${in_list}" -eq 1 ]]; then
            if [[ "${line}" =~ ^[[:space:]]+-[[:space:]]\"?([^\"]+)\"?[[:space:]]*$ ]]; then
                echo "${BASH_REMATCH[1]}"
            elif [[ ! "${line}" =~ ^[[:space:]]+-[[:space:]] && "${line}" =~ [^[:space:]] ]]; then
                in_list=0
            fi
        fi
    done < "${yaml_file}"
}

mapfile -t COMMON_PATCH_LIST < <(read_yaml_list "${CONFIG_YAML}" "php_patches")
mapfile -t VERSION_PATCH_LIST < <(read_yaml_list "${CONFIG_YAML}" "version_patches")

patch_list_fingerprint() {
    local patch_dir="$1"
    shift
    local names=("$@")
    local name
    for name in "${names[@]+"${names[@]}"}"; do
        [[ -z "${name}" ]] && continue
        local patch_file="${patch_dir}/${name}.patch"
        if [[ ! -f "${patch_file}" ]]; then
            echo "missing:${patch_file}"
            continue
        fi
        sha256sum "${patch_file}"
    done
}

PATCH_FINGERPRINT="$(
    {
        sha256sum "${CONFIG_YAML}"
        patch_list_fingerprint "${COMMON_PATCHES_DIR}" "${COMMON_PATCH_LIST[@]+"${COMMON_PATCH_LIST[@]}"}"
        patch_list_fingerprint "${VERSION_DIR}/patches" "${VERSION_PATCH_LIST[@]+"${VERSION_PATCH_LIST[@]}"}"
    } | sha256sum | awk '{print substr($1,1,16)}'
)"

PATCH_MARKER="${PHP_SRC}/.php-wasm-patches-applied-${VERSION}-${PATCH_FINGERPRINT}"
if [[ -f "${PATCH_MARKER}" ]]; then
    echo "  Patches already applied (marker: ${PATCH_MARKER})"
    exit 0
fi

apply_patch() {
    local patch_file="$1"
    local patch_name
    patch_name=$(basename "${patch_file}")

    if [[ ! -f "${patch_file}" ]]; then
        echo "ERROR: Patch file not found: ${patch_file}"
        echo "  Check that the patch name in config.yaml matches the filename in patches/"
        exit 1
    fi

    echo "  Applying: ${patch_name}"

    local patch_exit=0
    patch \
        --strip=1 \
        --forward \
        --fuzz=5 \
        --ignore-whitespace \
        --no-backup-if-mismatch \
        --directory="${PHP_SRC}" \
        < "${patch_file}" 2>&1 || patch_exit=$?

    if [[ "${patch_exit}" -eq 0 ]]; then
        : # all good
    elif [[ "${patch_exit}" -eq 1 ]]; then
        # exit 1 = some hunks rejected; usually means the change is already present
        echo "  WARNING: ${patch_name} — some hunks rejected (likely already applied)"
        find "${PHP_SRC}" -name "*.rej" -delete 2>/dev/null || true
    else
        echo ""
        echo "ERROR: Patch format/IO error (exit ${patch_exit}): ${patch_name}"
        echo "  Patch file : ${patch_file}"
        echo "  PHP source : ${PHP_SRC}"
        echo "See docs/UPDATING_PATCHES.md for the patch refresh workflow."
        exit 1
    fi
}

PHP_COUNT=0
for patch_name in "${COMMON_PATCH_LIST[@]+"${COMMON_PATCH_LIST[@]}"}"; do
    [[ -z "${patch_name}" ]] && continue
    apply_patch "${COMMON_PATCHES_DIR}/${patch_name}.patch"
    PHP_COUNT=$((PHP_COUNT + 1))
done

VERSION_COUNT=0
for patch_name in "${VERSION_PATCH_LIST[@]+"${VERSION_PATCH_LIST[@]}"}"; do
    [[ -z "${patch_name}" ]] && continue
    apply_patch "${VERSION_DIR}/patches/${patch_name}.patch"
    VERSION_COUNT=$((VERSION_COUNT + 1))
done

touch "${PATCH_MARKER}"
echo "  All patches applied (${PHP_COUNT} php + ${VERSION_COUNT} version-specific)"
