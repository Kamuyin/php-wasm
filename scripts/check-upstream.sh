#!/usr/bin/env bash
# Usage: check-upstream.sh [--create-issue]
# Checks php.net for new PHP releases and compares with tracked versions/.
# Exits 0 if all versions are current; non-zero if updates are available.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CREATE_ISSUE=0
GITHUB_TOKEN="${GITHUB_TOKEN:-}"
GITHUB_REPO="${GITHUB_REPOSITORY:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --create-issue) CREATE_ISSUE=1; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

PHP_API="https://www.php.net/releases/index.php?json&version=8&max=20"

echo "Checking PHP upstream releases..."
echo "  API: ${PHP_API}"
echo ""

if command -v curl &>/dev/null; then
    RELEASES_JSON=$(curl -fsSL --retry 3 --connect-timeout 15 "${PHP_API}")
elif command -v wget &>/dev/null; then
    RELEASES_JSON=$(wget -qO- --tries=3 --timeout=15 "${PHP_API}")
else
    echo "ERROR: curl or wget required"
    exit 1
fi

if [[ -z "${RELEASES_JSON}" || "${RELEASES_JSON}" == "null" ]]; then
    echo "ERROR: Empty or null response from php.net API"
    exit 1
fi

declare -A UPDATES
UPDATES_FOUND=0

for MINOR_DIR in "${REPO_ROOT}/versions"/*/; do
    MINOR=$(basename "${MINOR_DIR}")
    SOURCE_YAML="${MINOR_DIR}/source.yaml"

    [[ ! -f "${SOURCE_YAML}" ]] && continue

    CURRENT=$(grep '^php_version:' "${SOURCE_YAML}" \
        | sed 's/php_version: *"//' | sed 's/"//')

    echo "  Checking PHP ${MINOR}.x (tracked: ${CURRENT})"

    # Extract the latest release for this major.minor from the JSON
    LATEST=$(RELEASES_JSON="${RELEASES_JSON}" python3 - "${MINOR}" <<'PYEOF'
import json
import os
import re
import sys

minor = sys.argv[1]
data = json.loads(os.environ["RELEASES_JSON"])
pat = re.compile(r'^' + re.escape(minor) + r'\.\d+$')
versions = [v for v in data.keys() if pat.match(v)]
versions.sort(key=lambda v: list(map(int, v.split('.'))), reverse=True)
print(versions[0] if versions else "", end="")
PYEOF
)

    if [[ -z "${LATEST}" ]]; then
        echo "    WARNING: No ${MINOR}.x entries found in API response"
        continue
    fi

    if [[ "${CURRENT}" == "${LATEST}" ]]; then
        echo "    Status: up to date (${CURRENT})"
    else
        echo "    Status: UPDATE AVAILABLE: ${CURRENT} -> ${LATEST}"
        UPDATES["${MINOR}"]="${CURRENT} -> ${LATEST}"
        UPDATES_FOUND=$((UPDATES_FOUND + 1))
    fi
done

echo ""

if [[ "${UPDATES_FOUND}" -eq 0 ]]; then
    echo "All tracked PHP versions are up to date."
    exit 0
fi

echo "Found ${UPDATES_FOUND} update(s) available:"
for minor in "${!UPDATES[@]}"; do
    echo "  PHP ${minor}: ${UPDATES[$minor]}"
done

if [[ "${CREATE_ISSUE}" -eq 0 ]]; then
    echo ""
    echo "To create a GitHub issue automatically, run:"
    echo "  $0 --create-issue"
    echo ""
    echo "GITHUB_TOKEN and GITHUB_REPOSITORY must be set."
    exit 1
fi

# Validate required env vars for issue creation
if [[ -z "${GITHUB_TOKEN}" ]]; then
    echo "ERROR: GITHUB_TOKEN is not set (required for --create-issue)"
    exit 1
fi
if [[ -z "${GITHUB_REPO}" ]]; then
    echo "ERROR: GITHUB_REPOSITORY is not set (e.g. owner/repo)"
    exit 1
fi

# Check for existing open upstream-update issues to avoid duplicates
OPEN_COUNT=$(curl -fsSL \
    -H "Authorization: token ${GITHUB_TOKEN}" \
    -H "Accept: application/vnd.github.v3+json" \
    "https://api.github.com/repos/${GITHUB_REPO}/issues?labels=upstream-update&state=open&per_page=5" \
    | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")

if [[ "${OPEN_COUNT}" -gt 0 ]]; then
    echo ""
    echo "An upstream-update issue is already open (${OPEN_COUNT} found). Skipping creation."
    exit 1
fi

# Build update table for issue body
UPDATE_TABLE=""
for minor in "${!UPDATES[@]}"; do
    UPDATE_TABLE+="| PHP ${minor} | ${UPDATES[$minor]} |"$'\n'
done

ISSUE_TITLE="PHP upstream update available ($(date +%Y-%m-%d))"
ISSUE_BODY="## PHP Upstream Version Update Available

The following PHP versions have new upstream releases:

| Branch | Update |
|--------|--------|
${UPDATE_TABLE}

## Update Checklist

For each updated version, follow \`docs/ADDING_VERSION.md\`:

- [ ] Update \`versions/X.Y/source.yaml\`: bump \`php_version\`, \`source_tag\`, and \`source_sha256\`
- [ ] Run \`scripts/fetch-source.sh X.Y /tmp/build\` to download and compute real sha256
- [ ] Run \`scripts/apply-patches.sh X.Y /tmp/build/php-src-X.Y.Z\` — watch for patch failures
- [ ] Fix any failing patches (see \`docs/UPDATING_PATCHES.md\`)
- [ ] Run \`make test VERSION=X.Y PROFILE=minimal\` — verify smoke tests pass
- [ ] Update \`versions/X.Y/config.yaml\` notes if needed
- [ ] Open PR with version bump and any patch fixes

_This issue was created automatically by the upstream-watch workflow._"

echo ""
echo "Creating GitHub issue: ${ISSUE_TITLE}"

ISSUE_JSON=$(python3 -c "
import json, sys
title = sys.argv[1]
body  = sys.argv[2]
print(json.dumps({'title': title, 'body': body, 'labels': ['upstream-update', 'automated']}))
" "${ISSUE_TITLE}" "${ISSUE_BODY}")

ISSUE_URL=$(curl -fsSL \
    -X POST \
    -H "Authorization: token ${GITHUB_TOKEN}" \
    -H "Accept: application/vnd.github.v3+json" \
    -H "Content-Type: application/json" \
    "https://api.github.com/repos/${GITHUB_REPO}/issues" \
    -d "${ISSUE_JSON}" \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('html_url','(no url)'))")

echo "Issue created: ${ISSUE_URL}"
exit 1
