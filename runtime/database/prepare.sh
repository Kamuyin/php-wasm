#!/usr/bin/env bash
# Build inputs only; the running worker never downloads code.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEST="${1:?usage: prepare.sh OUTPUT_DIRECTORY}"
ARCHIVE="${SQLITE_TRANSLATOR_ARCHIVE:-${HOME}/.cache/php-wasm/sqlite-database-integration.3.0.2.zip}"
SHA=1602e75577ad9b3a7e3e4a6a44a81b9541cdee2124d48928faf61c6fd3cd4f74
mkdir -p "$(dirname "${ARCHIVE}")" "${DEST}"
if [[ ! -f "${ARCHIVE}" ]]; then
  curl -fsSL --retry 3 https://downloads.wordpress.org/plugin/sqlite-database-integration.3.0.2.zip -o "${ARCHIVE}.partial"
  mv "${ARCHIVE}.partial" "${ARCHIVE}"
fi
printf '%s  %s\n' "${SHA}" "${ARCHIVE}" | sha256sum --check --status
TEMP="$(mktemp -d)"
trap 'rm -rf "${TEMP}"' EXIT
unzip -q "${ARCHIVE}" -d "${TEMP}"
mkdir -p "${DEST}/mysql"
cp -R "${TEMP}/sqlite-database-integration/wp-includes/database/." "${DEST}/mysql/"
cp "${TEMP}/sqlite-database-integration/LICENSE" "${DEST}/mysql/LICENSE"
cp "${ROOT}/runtime/database/worker.php" "${DEST}/worker.php"
printf '%s\n' 'SQLite Database Integration 3.0.2, GPL-2.0-or-later' "Archive SHA256: ${SHA}" > "${DEST}/mysql/UPSTREAM.txt"
