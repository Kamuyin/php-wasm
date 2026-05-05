#!/usr/bin/env bash
# Usage: run-smoke.sh --wasm <php.wasm> [--runner wazero|wasmtime] [--php-ini <path>]
# Runs all smoke tests against a php-cgi WASM binary.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

WASM_FILE=""
RUNNER="wazero"
PHP_INI=""
PASS=0
FAIL=0
SKIP=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --wasm)     WASM_FILE="$2"; shift 2 ;;
        --runner)   RUNNER="$2"; shift 2 ;;
        --php-ini)  PHP_INI="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [[ -z "${WASM_FILE}" ]]; then
    echo "Usage: $0 --wasm <php.wasm> [--runner wazero|wasmtime] [--php-ini <path>]"
    exit 1
fi

if [[ ! -f "${WASM_FILE}" ]]; then
    echo "ERROR: WASM file not found: ${WASM_FILE}"
    exit 1
fi

# Detect runner
run_php_wasm() {
    local script="$1"
    local extra_env="${2:-}"

    case "${RUNNER}" in
        wazero)
            RUNNER_BIN="${REPO_ROOT}/tests/runners/wazero/php-wasm-runner"
            if [[ ! -x "${RUNNER_BIN}" ]]; then
                echo "ERROR: wazero runner not built. Run: cd tests/runners/wazero && go build -o php-wasm-runner ."
                exit 1
            fi
            ${RUNNER_BIN} \
                --wasm "${WASM_FILE}" \
                --script "${script}" \
                ${PHP_INI:+--php-ini "${PHP_INI}"}
            ;;
        wasmtime)
            if ! command -v wasmtime &>/dev/null; then
                echo "ERROR: wasmtime not found in PATH"
                exit 1
            fi
            wasmtime run \
                --dir "${SCRIPT_DIR}::/" \
                --dir "$(dirname "${script}")::/scripts" \
                --env "SCRIPT_FILENAME=/scripts/$(basename "${script}")" \
                --env "REQUEST_METHOD=GET" \
                --env "QUERY_STRING=" \
                --env "CONTENT_LENGTH=0" \
                "${WASM_FILE}" -- -f "${script}"
            ;;
        *)
            echo "ERROR: Unknown runner '${RUNNER}'. Supported: wazero, wasmtime"
            exit 1
            ;;
    esac
}

echo "==> PHP WASM Smoke Tests"
echo "    WASM   : ${WASM_FILE} ($(du -sh "${WASM_FILE}" | cut -f1))"
echo "    Runner : ${RUNNER}"
echo ""

for test_file in "${SCRIPT_DIR}"/[0-9]*.php; do
    test_name=$(basename "${test_file}" .php)
    printf "  %-25s ... " "${test_name}"

    OUTPUT=$(run_php_wasm "${test_file}" 2>&1) || true
    EXIT_CODE=$?

    if echo "${OUTPUT}" | grep -q "^SKIP:"; then
        echo "SKIP"
        SKIP=$((SKIP + 1))
    elif [[ "${EXIT_CODE}" -ne 0 ]] || echo "${OUTPUT}" | grep -q "^FAIL:"; then
        echo "FAIL"
        echo "    Output: $(echo "${OUTPUT}" | head -5 | sed 's/^/    /')"
        FAIL=$((FAIL + 1))
    elif echo "${OUTPUT}" | grep -q "^OK:"; then
        echo "PASS"
        PASS=$((PASS + 1))
    else
        echo "PASS (no OK marker)"
        PASS=$((PASS + 1))
    fi
done

echo ""
echo "==> Results: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"

if [[ "${FAIL}" -gt 0 ]]; then
    exit 1
fi
exit 0
