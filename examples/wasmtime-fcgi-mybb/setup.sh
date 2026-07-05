#!/usr/bin/env bash
# Bootstrap MyBB for the wasmtime-fcgi-mybb example.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

MYBB_VERSION="${MYBB_VERSION:-1838}"
MYBB_RELEASE_URL="https://resources.mybb.com/downloads/mybb_${MYBB_VERSION}.zip"

WWW_DIR="${SCRIPT_DIR}/www"
DATA_DIR="${SCRIPT_DIR}/data"
CACHE_DIR="${HOME}/.cache/php-wasm"

mkdir -p "${CACHE_DIR}"

info()    { echo "==> $*"; }
success() { echo "    OK: $*"; }
skip()    { echo "    SKIP: $*"; }

require_cmd() {
    if ! command -v "$1" &>/dev/null; then
        echo "ERROR: '$1' not found. Please install it."
        exit 1
    fi
}

require_cmd curl
require_cmd unzip

if [[ -f "${WWW_DIR}/index.php" && -d "${WWW_DIR}/inc" ]]; then
    skip "MyBB already installed at www/ (delete www/ to re-download)"
else
    info "Downloading MyBB ${MYBB_VERSION}..."
    MYBB_ZIP="${CACHE_DIR}/mybb-${MYBB_VERSION}.zip"
    if [[ ! -f "${MYBB_ZIP}" ]]; then
        curl -fsSL -o "${MYBB_ZIP}" "${MYBB_RELEASE_URL}"
    fi

    info "Extracting MyBB to www/..."
    # Unzip into a temporary directory
    TMP_UNZIP="${SCRIPT_DIR}/tmp_mybb"
    rm -rf "${TMP_UNZIP}"
    mkdir -p "${TMP_UNZIP}"
    unzip -q "${MYBB_ZIP}" -d "${TMP_UNZIP}"
    
    # MyBB zip contains an "Upload" folder. We want the contents of that folder in www/
    mkdir -p "${WWW_DIR}"
    # Use shopt to move hidden files if any, though MyBB typically doesn't have them in Upload
    mv "${TMP_UNZIP}/Upload/"* "${WWW_DIR}/"
    rm -rf "${TMP_UNZIP}"
    
    # Patch my_chmod for WASI compatibility since chmod() is not available
    info "Patching MyBB for WASI compatibility..."
    sed -i 's/$result = chmod($file, octdec($mode));/if(!function_exists("chmod")) { $result = true; } else { $result = chmod($file, octdec($mode)); }/g' "${WWW_DIR}/inc/functions.php"

    success "MyBB ${MYBB_VERSION} extracted to www/"
fi

# --------------------------------------------------------------------------- #
# 2. Setup config files for the installer

info "Setting up configuration files for the installer..."
if [[ -f "${WWW_DIR}/inc/config.default.php" && ! -f "${WWW_DIR}/inc/config.php" ]]; then
    cp "${WWW_DIR}/inc/config.default.php" "${WWW_DIR}/inc/config.php"
    chmod 666 "${WWW_DIR}/inc/config.php"
    success "inc/config.php created and made writable"
else
    skip "inc/config.php already exists"
fi

if [[ ! -f "${WWW_DIR}/inc/settings.php" ]]; then
    touch "${WWW_DIR}/inc/settings.php"
    chmod 666 "${WWW_DIR}/inc/settings.php"
    success "inc/settings.php created and made writable"
fi

# --------------------------------------------------------------------------- #
# 3. Data directories for SQLite and uploads

info "Preparing data directories..."
mkdir -p "${DATA_DIR}/database"
mkdir -p "${WWW_DIR}/uploads/avatars"
mkdir -p "${WWW_DIR}/admin/backups"
# Ensure cache directories are writable
mkdir -p "${WWW_DIR}/cache/themes"
# Make sure installer can write to these
chmod 777 "${WWW_DIR}/uploads" "${WWW_DIR}/uploads/avatars" "${WWW_DIR}/admin/backups" "${WWW_DIR}/cache" "${WWW_DIR}/cache/themes" 2>/dev/null || true
success "Data directories ready"

echo ""
echo "==> Setup complete!"
echo ""
echo "Next: start the server and open the MyBB installer"
echo ""
echo "  go run . --wasm ../../out/php-8.3.30-wordpress.wasm"
echo "  open http://localhost:8080/install/"
echo ""
echo "Notes:"
echo "  - When prompted for database details, select 'SQLite 3'"
echo "  - Set the Database Path to: /data/database/mybb.sqlite"
echo "  - To reset everything: rm -rf www/ data/ && ./setup.sh"
