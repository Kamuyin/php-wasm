# Adding a New PHP Version

This guide covers adding a new PHP version (e.g. 8.5) to the build system.

## Prerequisites

- You have read `docs/ARCHITECTURE.md`
- The new PHP version is released on [php.net](https://www.php.net/releases/)
- You have a local build environment (wasi-sdk 24+ or Docker)

## Step 1: Create the version directory

```bash
VERSION=8.5
mkdir -p versions/${VERSION}/patches
```

## Step 2: Create source.yaml

```bash
# Find the exact version number (e.g. 8.5.0)
EXACT_VERSION="8.5.0"

cat > versions/${VERSION}/source.yaml <<EOF
# \$schema: ../../schemas/source.schema.json
php_version: "${EXACT_VERSION}"
source_tag: "php-${EXACT_VERSION}"
source_sha256: "PLACEHOLDER_SHA256_REPLACE_BEFORE_BUILD"
download_url: "https://github.com/php/php-src/archive/refs/tags/php-${EXACT_VERSION}.tar.gz"
download_url_alt: "https://www.php.net/distributions/php-${EXACT_VERSION}.tar.gz"
branch: "PHP-${VERSION}"
eol_date: "2027-12-31"
security_only: false
EOF
```

## Step 3: Download the source and get the real sha256

```bash
./scripts/fetch-source.sh ${VERSION} /tmp/php-wasm-new

# The script will print: "WARNING: sha256 is a placeholder"
# And show the tarball path. Get the real hash:
sha256sum /tmp/php-wasm-new/php-${EXACT_VERSION}.tar.gz

# Update source.yaml with the real hash
sed -i "s/PLACEHOLDER_SHA256_REPLACE_BEFORE_BUILD/<actual-sha256>/" \
    versions/${VERSION}/source.yaml

# Verify
./scripts/fetch-source.sh ${VERSION} /tmp/php-wasm-new2
```

## Step 4: Create config.yaml

Start by copying from the nearest existing version:

```bash
cp versions/8.4/config.yaml versions/${VERSION}/config.yaml
```

Update `php_version` and `source_tag`, then verify the `common_patches` list still
applies (see Step 5).

## Step 5: Test patch application

```bash
./scripts/apply-patches.sh ${VERSION} /tmp/php-wasm-new/php-src-php-${EXACT_VERSION}
```

If patches fail:

- **Patch already applied upstream** — remove it from `common_patches` in config.yaml
- **Patch needs refresh** — see `docs/UPDATING_PATCHES.md`
- **New issue introduced** — write a new version-specific patch in `versions/${VERSION}/patches/`

## Step 6: Do a test build

```bash
# Docker build (recommended for reproducibility)
make docker-build VERSION=${VERSION} PROFILE=minimal

# Or direct build
./scripts/build.sh ${VERSION} minimal --output-dir ./out
```

## Step 7: Run smoke tests

```bash
# Build the wazero test runner first
cd tests/runners/wazero && go build -o php-wasm-runner . && cd ../../..

./tests/smoke/run-smoke.sh \
    --wasm out/php-${EXACT_VERSION}-minimal.wasm \
    --runner wazero
```

All tests should pass. If any fail, check the smoke test output and fix patches.

## Step 8: Add to CI matrix

Edit `.github/workflows/build.yml` and add the new version to the matrix:

```yaml
strategy:
  matrix:
    php: ["8.2", "8.3", "8.4", "8.5"]  # add here
```

## Step 9: Open a PR

```bash
git add versions/${VERSION}/
git commit -m "Add PHP ${EXACT_VERSION} support"
```

The PR description should include:
- The PHP changelog link
- Whether any common patches needed updating
- Smoke test results
- Any new limitations discovered
