#!/usr/bin/env bash
# Usage: package.sh <wasm-file> [--registry ghcr.io] [--sign]
# Pushes a .wasm binary as an OCI artifact via oras-cli and optionally signs with cosign.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

WASM_FILE="${1:-}"
REGISTRY="${REGISTRY:-ghcr.io}"
REPO_OWNER="${GITHUB_REPOSITORY_OWNER:-}"
SIGN=0

if [[ -z "${WASM_FILE}" ]]; then
    echo "Usage: $0 <wasm-file> [--registry REGISTRY] [--sign]"
    echo ""
    echo "  wasm-file   Path to a .wasm artifact (e.g. out/php-8.3.30-default.wasm)"
    echo "  --registry  OCI registry hostname (default: ghcr.io)"
    echo "  --sign      Sign the pushed artifact with cosign (keyless OIDC)"
    echo ""
    echo "Environment variables:"
    echo "  REGISTRY                  Override OCI registry"
    echo "  GITHUB_REPOSITORY_OWNER  GitHub org/user for the image path"
    echo "  GITHUB_TOKEN             Token for registry authentication"
    exit 1
fi

shift
while [[ $# -gt 0 ]]; do
    case "$1" in
        --registry) REGISTRY="$2"; shift 2 ;;
        --sign)     SIGN=1; shift ;;
        *) echo "ERROR: Unknown option: $1"; exit 1 ;;
    esac
done

if [[ ! -f "${WASM_FILE}" ]]; then
    echo "ERROR: Artifact not found: ${WASM_FILE}"
    exit 1
fi

if ! command -v oras &>/dev/null; then
    echo "ERROR: oras not found in PATH"
    echo "  Install from https://oras.land or:"
    echo "  brew install oras"
    exit 1
fi

if [[ -z "${REPO_OWNER}" ]]; then
    echo "ERROR: GITHUB_REPOSITORY_OWNER is not set"
    echo "  Set it to your GitHub username or organisation"
    exit 1
fi

# Parse artifact filename: php-8.3.30-default.wasm
ARTIFACT_BASE=$(basename "${WASM_FILE}" .wasm)
# Extract PHP_VERSION: everything between "php-" and the last "-<profile>"
PROFILE_NAME=$(echo "${ARTIFACT_BASE}" | rev | cut -d- -f1 | rev)
PHP_VERSION=$(echo "${ARTIFACT_BASE}" | sed "s/^php-//" | sed "s/-${PROFILE_NAME}$//")

if [[ -z "${PHP_VERSION}" || -z "${PROFILE_NAME}" ]]; then
    echo "ERROR: Could not parse version/profile from filename: ${ARTIFACT_BASE}"
    echo "  Expected format: php-<version>-<profile>.wasm"
    exit 1
fi

IMAGE_BASE="${REGISTRY}/${REPO_OWNER}/php-wasm"
IMAGE_REF="${IMAGE_BASE}:${PHP_VERSION}-${PROFILE_NAME}"
IMAGE_LATEST="${IMAGE_BASE}:latest-${PROFILE_NAME}"

# Generate sha256 if missing
SHA256_FILE="${WASM_FILE}.sha256"
if [[ ! -f "${SHA256_FILE}" ]]; then
    sha256sum "${WASM_FILE}" > "${SHA256_FILE}"
fi

WASM_DIGEST=$(awk '{print $1}' "${SHA256_FILE}")
WASM_SIZE=$(du -sh "${WASM_FILE}" | cut -f1)

echo "  Pushing OCI artifact"
echo "  Image  : ${IMAGE_REF}"
echo "  File   : ${WASM_FILE} (${WASM_SIZE})"
echo "  Digest : sha256:${WASM_DIGEST}"

oras push "${IMAGE_REF}" \
    --artifact-type "application/vnd.php-wasm.binary.v1" \
    --annotation "org.opencontainers.image.title=php-wasm" \
    --annotation "org.opencontainers.image.version=${PHP_VERSION}" \
    --annotation "org.opencontainers.image.source=https://github.com/${REPO_OWNER}/php-wasm" \
    --annotation "php-wasm.profile=${PROFILE_NAME}" \
    --annotation "php-wasm.target=wasm32-wasi" \
    --annotation "php-wasm.digest=sha256:${WASM_DIGEST}" \
    "${WASM_FILE}:application/vnd.wasm.content.layer.v1+wasm" \
    "${SHA256_FILE}:application/vnd.php-wasm.sha256.v1"

echo "  Pushed: ${IMAGE_REF}"

# Tag as latest-<profile>
oras tag "${IMAGE_REF}" "${IMAGE_LATEST}" 2>/dev/null || \
    echo "  WARNING: Could not tag as ${IMAGE_LATEST}"

if [[ "${SIGN}" -eq 1 ]]; then
    if ! command -v cosign &>/dev/null; then
        echo "ERROR: cosign not found in PATH"
        echo "  Install from https://docs.sigstore.dev/cosign/installation/"
        exit 1
    fi
    echo "  Signing with cosign (keyless OIDC)..."
    # Fetch the digest of the just-pushed manifest
    MANIFEST_DIGEST=$(oras manifest fetch "${IMAGE_REF}" --descriptor \
        | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['digest'])")
    cosign sign --yes "${IMAGE_BASE}@${MANIFEST_DIGEST}"
    echo "  Signed: ${IMAGE_BASE}@${MANIFEST_DIGEST}"
fi

echo "  Package complete: ${IMAGE_REF}"
