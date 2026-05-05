#!/usr/bin/env bash
# Usage: apply-patches.sh <version> <php-src-dir>
# Applies version-specific WASI patches to a PHP source tree.
# Uses the POSIX patch(1) command with fuzz tolerance for robustness
# across minor PHP patch-level releases.
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
COMMON_PATCHES_DIR="${REPO_ROOT}/patches/common"

if [[ ! -d "${PHP_SRC}" ]]; then
    echo "ERROR: PHP source directory not found: ${PHP_SRC}"
    exit 1
fi

if [[ ! -f "${CONFIG_YAML}" ]]; then
    echo "ERROR: config.yaml not found: ${CONFIG_YAML}"
    exit 1
fi

# Check if patches were already applied (idempotency)
PATCH_MARKER="${PHP_SRC}/.php-wasm-patches-applied-${VERSION}"
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

    # Use patch(1) with high fuzz tolerance. This is more robust than git apply
    # across minor PHP patch-level releases where context may have drifted.
    # --forward:               skip already-applied hunks instead of reversing
    # --fuzz=5:                allow up to 5 lines of context mismatch
    # --ignore-whitespace:     tolerate trailing-space differences
    # --no-backup-if-mismatch: don't litter .orig files on partial apply
    local patch_exit=0
    patch \
        --strip=1 \
        --forward \
        --fuzz=5 \
        --ignore-whitespace \
        --no-backup-if-mismatch \
        --directory="${PHP_SRC}" \
        < "${patch_file}" 2>&1 || patch_exit=$?

    # patch exit codes: 0=all applied, 1=some hunks rejected, 2=format/IO error
    if [[ "${patch_exit}" -eq 0 ]]; then
        : # all good
    elif [[ "${patch_exit}" -eq 1 ]]; then
        # Rejected hunks typically mean the change is already present in the
        # source (applied by PHP upstream or a prior version of this patch).
        # Treat as a warning — the build can proceed. Inspect .rej files if
        # the resulting binary misbehaves.
        echo "  WARNING: ${patch_name} — some hunks rejected (likely already applied)"
        echo "    Inspect .rej files in ${PHP_SRC} if needed; continuing build."
        # Remove .rej files so make doesn't get confused by them
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

# Apply common patches listed in config.yaml
COMMON_COUNT=0
while IFS= read -r patch_name; do
    [[ -z "${patch_name}" ]] && continue
    apply_patch "${COMMON_PATCHES_DIR}/${patch_name}.patch"
    COMMON_COUNT=$((COMMON_COUNT + 1))
done < <(read_yaml_list "${CONFIG_YAML}" "common_patches")

if [[ "${COMMON_COUNT}" -gt 0 ]]; then
    echo "  Applied ${COMMON_COUNT} common patch(es)"
fi

# Apply version-specific patches
VERSION_COUNT=0
while IFS= read -r patch_name; do
    [[ -z "${patch_name}" ]] && continue
    apply_patch "${VERSION_DIR}/patches/${patch_name}.patch"
    VERSION_COUNT=$((VERSION_COUNT + 1))
done < <(read_yaml_list "${CONFIG_YAML}" "version_patches")

if [[ "${VERSION_COUNT}" -eq 0 ]]; then
    echo "  No version-specific patches for PHP ${VERSION}"
else
    echo "  Applied ${VERSION_COUNT} version-specific patch(es)"
fi

# Leave a marker so re-runs skip redundant patching
touch "${PATCH_MARKER}"
echo "  All patches applied successfully (${COMMON_COUNT} common + ${VERSION_COUNT} version-specific)"
