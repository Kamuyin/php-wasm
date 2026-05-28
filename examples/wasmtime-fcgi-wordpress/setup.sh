#!/usr/bin/env bash
# Bootstrap WordPress + sqlite-database-integration for the wazero-wordpress example.
#
# What this script does:
#   1. Downloads WordPress (pinned version) to www/
#   2. Downloads sqlite-database-integration plugin
#   3. Installs the SQLite drop-in (wp-content/db.php)
#   4. Creates data/database/ and data/uploads/ directories
#   5. Generates wp-config.php with secure random salts and WASI-safe settings
#
# Idempotent: skips downloads if www/wp-load.php already exists.
# Override WP_VERSION, SQLITE_PLUGIN_VERSION, or WP_TARBALL_PATH to customise.
#
# Usage:
#   ./setup.sh
#   WP_VERSION=6.7.1 ./setup.sh
#   WP_TARBALL_PATH=/path/to/wordpress.tar.gz ./setup.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

WP_VERSION="${WP_VERSION:-6.6.2}"
SQLITE_PLUGIN_VERSION="${SQLITE_PLUGIN_VERSION:-2.1.11}"
WP_HOME="${WP_HOME:-http://localhost:8080}"

# Directory names
WWW_DIR="${SCRIPT_DIR}/www"
DATA_DIR="${SCRIPT_DIR}/data"
PLUGINS_DIR="${WWW_DIR}/wp-content/plugins"
SQLITE_PLUGIN_DIR="${PLUGINS_DIR}/sqlite-database-integration"

CACHE_DIR="${HOME}/.cache/php-wasm"
mkdir -p "${CACHE_DIR}"

# --------------------------------------------------------------------------- #
# helpers

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
require_cmd openssl

# --------------------------------------------------------------------------- #
# 1. WordPress core

if [[ -f "${WWW_DIR}/wp-load.php" ]]; then
    skip "WordPress already installed at www/ (delete www/ to re-download)"
else
    info "Downloading WordPress ${WP_VERSION}..."

    if [[ -n "${WP_TARBALL_PATH:-}" ]]; then
        WP_TARBALL="${WP_TARBALL_PATH}"
    else
        WP_TARBALL="${CACHE_DIR}/wordpress-${WP_VERSION}.tar.gz"
        if [[ ! -f "${WP_TARBALL}" ]]; then
            curl -fsSL \
                -o "${WP_TARBALL}" \
                "https://wordpress.org/wordpress-${WP_VERSION}.tar.gz"
        fi
    fi

    info "Extracting WordPress to www/..."
    mkdir -p "${SCRIPT_DIR}"
    tar -xzf "${WP_TARBALL}" -C "${SCRIPT_DIR}"
    # tar extracts to a 'wordpress/' subdirectory — rename it to www/
    mv "${SCRIPT_DIR}/wordpress" "${WWW_DIR}"
    success "WordPress ${WP_VERSION} extracted to www/"
fi

# --------------------------------------------------------------------------- #
# 2. sqlite-database-integration plugin

if [[ -d "${SQLITE_PLUGIN_DIR}" ]]; then
    skip "sqlite-database-integration already installed"
else
    info "Downloading sqlite-database-integration ${SQLITE_PLUGIN_VERSION}..."
    SQLITE_PLUGIN_ZIP="${CACHE_DIR}/sqlite-database-integration.${SQLITE_PLUGIN_VERSION}.zip"
    if [[ ! -f "${SQLITE_PLUGIN_ZIP}" ]]; then
        curl -fsSL \
            -o "${SQLITE_PLUGIN_ZIP}" \
            "https://downloads.wordpress.org/plugin/sqlite-database-integration.zip"
    fi
    mkdir -p "${PLUGINS_DIR}"
    unzip -q "${SQLITE_PLUGIN_ZIP}" -d "${PLUGINS_DIR}"
    success "sqlite-database-integration ${SQLITE_PLUGIN_VERSION} installed"
fi

# --------------------------------------------------------------------------- #
# 3. SQLite drop-in: db.php

DB_COPY="${SQLITE_PLUGIN_DIR}/db.copy"
DB_PHP="${WWW_DIR}/wp-content/db.php"

if [[ -f "${DB_PHP}" ]]; then
    skip "wp-content/db.php already exists"
elif [[ ! -f "${DB_COPY}" ]]; then
    echo "ERROR: ${DB_COPY} not found — sqlite-database-integration plugin may be corrupt"
    exit 1
else
    info "Installing SQLite drop-in (wp-content/db.php)..."
    cp "${DB_COPY}" "${DB_PHP}"
    # The plugin ships db.copy with placeholder tokens. Replace them so db.php
    # knows where to find the implementation.
    sed -i \
        "s|{SQLITE_IMPLEMENTATION_FOLDER_NAME}|sqlite-database-integration|g" \
        "${DB_PHP}"
    sed -i \
        "s|{SQLITE_FOLDER}|database/|g" \
        "${DB_PHP}"
    success "wp-content/db.php installed"
fi

# --------------------------------------------------------------------------- #
# 4. Data directories (persistent across requests via wazero mounts)
# data/ is mounted at /data inside WASM — a flat separate pre-open rather
# than a nested mount under /srv/app to avoid wazero path-routing issues.
# wp-config.php points DB_DIR at /data/database/ (absolute WASI path).

mkdir -p "${DATA_DIR}/database" "${DATA_DIR}/uploads"
success "data/database/ and data/uploads/ ready"

# --------------------------------------------------------------------------- #
# 5. Must-use plugin: WASI compatibility shim

MU_DIR="${WWW_DIR}/wp-content/mu-plugins"
MU_PLUGIN="${MU_DIR}/wasi-compat.php"

if [[ -f "${MU_PLUGIN}" ]]; then
    skip "mu-plugins/wasi-compat.php already exists"
else
    info "Installing WASI compatibility mu-plugin..."
    mkdir -p "${MU_DIR}"
    cat > "${MU_PLUGIN}" << 'MUPLUGIN'
<?php
/**
 * WASI compatibility shim (auto-loaded must-use plugin).
 *
 * WASI Preview1 has no network sockets. WordPress's installer calls
 * wp_install_maybe_enable_pretty_permalinks(), which issues a loopback
 * HTTP request that would reach Fsockopen and crash with a fatal
 * "undefined function stream_socket_client()" error.
 *
 * This filter runs inside WP_Http::request() *before* a transport is
 * selected, so it prevents the crash entirely. It returns a fake 200
 * for loopback calls (so WP enables pretty permalinks) and a WP_Error
 * for all other outbound requests (so WP degrades gracefully instead of
 * hanging or crashing).
 */
add_filter( 'pre_http_request', static function ( $preempt, $args, $url ) {
    if ( false !== $preempt ) {
        return $preempt; // already preempted upstream
    }

    // Loopback requests (installer permalink test, health checks, etc.)
    // Return a fake 200 so WordPress believes URL rewriting works.
    if ( preg_match( '#^https?://(localhost|127\.0\.0\.1)(:\d+)?(/|$)#', $url ) ) {
        return [
            'headers'       => [],
            'body'          => '',
            'response'      => [ 'code' => 200, 'message' => 'OK' ],
            'cookies'       => [],
            'http_response' => null,
        ];
    }

    // All other outbound requests: return a clear error rather than a fatal crash.
    return new WP_Error(
        'http_request_failed',
        'WASI Preview1: no outbound network sockets. Outbound HTTP is not available.'
    );
}, 10, 3 );
MUPLUGIN
    success "mu-plugins/wasi-compat.php installed"
fi

# --------------------------------------------------------------------------- #
# 6. wp-config.php

WP_CONFIG="${WWW_DIR}/wp-config.php"

if [[ -f "${WP_CONFIG}" ]]; then
    skip "wp-config.php already exists"
else
    info "Generating wp-config.php with random salts..."

    # Generate 8 independent salt values
    AUTH_KEY=$(openssl rand -hex 32)
    SECURE_AUTH_KEY=$(openssl rand -hex 32)
    LOGGED_IN_KEY=$(openssl rand -hex 32)
    NONCE_KEY=$(openssl rand -hex 32)
    AUTH_SALT=$(openssl rand -hex 32)
    SECURE_AUTH_SALT=$(openssl rand -hex 32)
    LOGGED_IN_SALT=$(openssl rand -hex 32)
    NONCE_SALT=$(openssl rand -hex 32)

    cat > "${WP_CONFIG}" << PHP
<?php
/**
 * WordPress configuration for WASI/SQLite builds.
 * Generated by examples/wazero-wordpress/setup.sh — do not commit.
 *
 * The sqlite-database-integration plugin intercepts \$wpdb calls via
 * wp-content/db.php. No MySQL connection is made; all data lives in
 * data/database/.ht.sqlite, which the Go host mounts at /data/database/
 * inside the WASM sandbox (separate from the docroot at /srv/app).
 */

// --- Database (SQLite via drop-in; the values below are unused placeholders) ---
define( 'DB_NAME',     'wordpress' );
define( 'DB_USER',     'root' );
define( 'DB_PASSWORD', '' );
define( 'DB_HOST',     'localhost' );
define( 'DB_CHARSET',  'utf8' );
define( 'DB_COLLATE',  '' );

// Absolute WASI path for the SQLite database directory.
// data/ is mounted at /data by the Go host; this must be defined BEFORE
// wp-settings.php loads the db.php drop-in so the plugin uses our path.
if ( ! defined( 'DB_DIR' ) ) {
    define( 'DB_DIR', '/data/database/' );
}

// --- Auth keys and salts ---
define( 'AUTH_KEY',         '${AUTH_KEY}' );
define( 'SECURE_AUTH_KEY',  '${SECURE_AUTH_KEY}' );
define( 'LOGGED_IN_KEY',    '${LOGGED_IN_KEY}' );
define( 'NONCE_KEY',        '${NONCE_KEY}' );
define( 'AUTH_SALT',        '${AUTH_SALT}' );
define( 'SECURE_AUTH_SALT', '${SECURE_AUTH_SALT}' );
define( 'LOGGED_IN_SALT',   '${LOGGED_IN_SALT}' );
define( 'NONCE_SALT',       '${NONCE_SALT}' );

// --- Table prefix ---
\$table_prefix = 'wp_';

// --- WASI-safe settings ---

// Disable wp-cron (no outbound HTTP from WASI). Trigger /wp-cron.php from the
// host instead: */1 * * * * curl -s http://localhost:8080/wp-cron.php?doing_wp_cron
define( 'DISABLE_WP_CRON', true );

// No plugin/theme auto-updates — WASI has no outbound network.
define( 'AUTOMATIC_UPDATER_DISABLED', true );

// Block outbound HTTP requests (curl/fsockopen not available in WASI Preview1).
define( 'WP_HTTP_BLOCK_EXTERNAL', true );

// Direct filesystem access (no FTP/SSH).
define( 'FS_METHOD', 'direct' );

// Site URLs — update if running on a non-localhost address.
define( 'WP_HOME',    '${WP_HOME}' );
define( 'WP_SITEURL', '${WP_HOME}' );

// Set to true to enable wp-admin's debug bar and error display.
define( 'WP_DEBUG',         false );
define( 'WP_DEBUG_LOG',     false );
define( 'WP_DEBUG_DISPLAY', false );

// --- Absolute path ---
if ( ! defined( 'ABSPATH' ) ) {
    define( 'ABSPATH', __DIR__ . '/' );
}

require_once ABSPATH . 'wp-settings.php';
PHP

    success "wp-config.php generated"
fi

# --------------------------------------------------------------------------- #

echo ""
echo "==> Setup complete!"
echo ""
echo "Next: start the server and open the WordPress installer"
echo ""
echo "  go run . --wasm ../../out/php-8.3.30-wordpress.wasm"
echo "  open http://localhost:8080/wp-admin/install.php"
echo ""
echo "Notes:"
echo "  - The SQLite database will be created at data/database/.ht.sqlite on"
echo "    first request. It persists across server restarts."
echo "  - Uploads go to data/uploads/ (mounted at /srv/app/wp-content/uploads)."
echo "  - To reset everything: rm -rf www/ data/ && ./setup.sh"
