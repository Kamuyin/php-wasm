#!/usr/bin/env bash
# Bootstrap Drupal for the wazero-drupal example.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

DRUPAL_VERSION="${DRUPAL_VERSION:-10.3.6}"
SITE_URL="${SITE_URL:-http://localhost:8080}"

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
require_cmd tar
require_cmd openssl

if [[ -f "${WWW_DIR}/core/lib/Drupal.php" ]]; then
    skip "Drupal already installed at www/ (delete www/ to re-download)"
else
    info "Downloading Drupal ${DRUPAL_VERSION}..."
    if [[ -n "${DRUPAL_TARBALL_PATH:-}" ]]; then
        DRUPAL_TARBALL="${DRUPAL_TARBALL_PATH}"
    else
        DRUPAL_TARBALL="${CACHE_DIR}/drupal-${DRUPAL_VERSION}.tar.gz"
        if [[ ! -f "${DRUPAL_TARBALL}" ]]; then
            curl -fsSL \
                -o "${DRUPAL_TARBALL}" \
                "https://ftp.drupal.org/files/projects/drupal-${DRUPAL_VERSION}.tar.gz"
        fi
    fi

    info "Extracting Drupal to www/..."
    rm -rf "${SCRIPT_DIR}/drupal-${DRUPAL_VERSION}"
    tar -xzf "${DRUPAL_TARBALL}" -C "${SCRIPT_DIR}"
    mv "${SCRIPT_DIR}/drupal-${DRUPAL_VERSION}" "${WWW_DIR}"
    success "Drupal ${DRUPAL_VERSION} extracted to www/"
fi

PATCHES_DIR="${SCRIPT_DIR}/patches/drupal-${DRUPAL_VERSION%.*}"
PATCH_MARKER="${WWW_DIR}/.wasi-patches-applied-${DRUPAL_VERSION%.*}"

if [[ -f "${PATCH_MARKER}" ]]; then
    skip "WASI patches already applied"
elif [[ -d "${PATCHES_DIR}" ]]; then
    info "Applying WASI compatibility patches..."
    for patch_file in "${PATCHES_DIR}"/*.patch; do
        [[ -f "${patch_file}" ]] || continue
        echo "  Applying: $(basename "${patch_file}")"
        rc=0
        patch --strip=1 --forward --fuzz=5 --ignore-whitespace \
              --no-backup-if-mismatch --directory="${WWW_DIR}" \
              < "${patch_file}" || rc=$?
        if [[ "${rc}" -eq 1 ]]; then
            find "${WWW_DIR}" -name "*.rej" -delete 2>/dev/null || true
            echo "  WARNING: some hunks rejected (likely already applied)"
        elif [[ "${rc}" -gt 1 ]]; then
            echo "ERROR: patch failed (exit ${rc}): $(basename "${patch_file}")"
            echo "  If Drupal was upgraded, regenerate patches from the new version."
            exit 1
        fi
    done
    touch "${PATCH_MARKER}"
    success "WASI patches applied"
fi

info "Preparing writable directories..."
mkdir -p "${WWW_DIR}/sites/default/files"
mkdir -p "${DATA_DIR}/database"
mkdir -p "${DATA_DIR}/private"
mkdir -p "${DATA_DIR}/config/sync"
chmod 755 "${WWW_DIR}/sites/default/files"
success "Writable directories ready"

SETTINGS_FILE="${WWW_DIR}/sites/default/settings.php"
DEFAULT_SETTINGS="${WWW_DIR}/sites/default/default.settings.php"
MARKER="php-wasm-drupal-sqlite"

if [[ ! -f "${SETTINGS_FILE}" ]]; then
    if [[ ! -f "${DEFAULT_SETTINGS}" ]]; then
        echo "ERROR: ${DEFAULT_SETTINGS} not found"
        exit 1
    fi
    info "Creating sites/default/settings.php..."
    cp "${DEFAULT_SETTINGS}" "${SETTINGS_FILE}"
    success "settings.php created"
fi

if grep -q "${MARKER}" "${SETTINGS_FILE}"; then
    skip "settings.php already contains WASI/SQLite configuration"
else
    info "Appending WASI/SQLite Drupal settings..."
    HASH_SALT="$(openssl rand -hex 32)"
    cat >> "${SETTINGS_FILE}" << PHP

/**
 * ${MARKER}
 * Added by examples/wazero-drupal/setup.sh
 */
\$settings['hash_salt'] = '${HASH_SALT}';
\$databases['default']['default'] = [
    'driver' => 'sqlite',
    'database' => '/data/database/drupal.sqlite',
    'prefix' => '',
];
\$settings['file_private_path'] = '/data/private';
\$settings['config_sync_directory'] = '/data/config/sync';
\$settings['skip_permissions_hardening'] = TRUE;
\$settings['trusted_host_patterns'] = [
    '^localhost$',
    '^127\\.0\\.0\\.1$',
    '^\\[::1\\]$',
];
\$settings['update_fetch_with_http_fallback'] = FALSE;
\$settings['file_temp_path'] = '/tmp';
\$config['system.performance']['css']['preprocess'] = FALSE;
\$config['system.performance']['js']['preprocess'] = FALSE;
PHP
    success "WASI/SQLite settings appended"
fi

mkdir -p "${WWW_DIR}/sites/default/files/translations"
mkdir -p "${WWW_DIR}/sites/default/files/css"
mkdir -p "${WWW_DIR}/sites/default/files/js"
mkdir -p "${WWW_DIR}/sites/default/files/php"
mkdir -p "${WWW_DIR}/sites/default/files/styles"
# WASI path_create_directory is single-level; pre-create multi-level paths the installer needs.
mkdir -p "${WWW_DIR}/sites/default/files/media-icons/generic"

echo ""
echo "==> Setup complete!"
echo ""
echo "Next: start the server and open the Drupal installer:"
echo ""
echo "  go run . --wasm ../../out/php-8.3.30-drupal.wasm"
echo "  open ${SITE_URL}/core/install.php"
echo ""
echo "Notes:"
echo "  - SQLite DB path inside WASM: /data/database/drupal.sqlite"
echo "  - Public files directory: www/sites/default/files"
echo "  - To reset everything: rm -rf www/ data/ && ./setup.sh"
