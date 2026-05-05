# Building php-wasm Locally

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Docker | 24+ | For reproducible builds |
| wasi-sdk | 24+ | Only needed for direct (non-Docker) builds |
| Go | 1.22+ | Only needed to build/run the test runner |
| make | any | Convenience wrapper |

## Quickstart (Docker)

The fastest path to a working binary:

```bash
# Clone
git clone https://github.com/your-org/php-wasm
cd php-wasm

# Build PHP 8.3 with the default profile
make docker-build VERSION=8.3 PROFILE=default

# Find the output
ls -lh out/php-8.3.*
```

## Quickstart (without Docker)

Requires wasi-sdk installed at `/opt/wasi-sdk` (or set `WASI_SDK_PATH`):

```bash
# Build
./scripts/build.sh 8.3 default --output-dir ./out

# With wasm-opt optimization (requires wasm-opt in PATH)
./scripts/build.sh 8.3 default --output-dir ./out
```

## Step-by-Step Manual Build

### 1. Install wasi-sdk

```bash
WASI_SDK_VERSION=24
curl -Lo /tmp/wasi-sdk.tar.gz \
  "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-${WASI_SDK_VERSION}/wasi-sdk-${WASI_SDK_VERSION}.0-x86_64-linux.tar.gz"
sudo mkdir -p /opt/wasi-sdk
sudo tar -xzf /tmp/wasi-sdk.tar.gz -C /opt/wasi-sdk --strip-components=1
```

### 2. Fetch PHP source

```bash
# Downloads and verifies sha256
./scripts/fetch-source.sh 8.3 /tmp/php-wasm-build
```

After running this, update the `source_sha256` in `versions/8.3/source.yaml`
with the actual hash (the script will print it if the placeholder is present).

### 3. Apply patches

```bash
./scripts/apply-patches.sh 8.3 /tmp/php-wasm-build/php-src-php-8.3.30
```

If a patch fails, see `docs/UPDATING_PATCHES.md`.

### 4. Configure

```bash
./scripts/configure-php.sh 8.3 default /tmp/php-wasm-build/php-src-php-8.3.30
```

### 5. Build

```bash
make -C /tmp/php-wasm-build/php-src-php-8.3.30 -j$(nproc)
```

### 6. Strip and package

```bash
WASM_BIN=/tmp/php-wasm-build/php-src-php-8.3.30/sapi/cgi/php-cgi
llvm-strip --strip-debug "${WASM_BIN}" -o out/php-8.3.30-default.wasm

# Optional: optimize with wasm-opt
wasm-opt -Oz out/php-8.3.30-default.wasm -o out/php-8.3.30-default.wasm
```

## Build Profiles

| Profile | Extensions | Approx. Size |
|---------|-----------|-------------|
| `minimal` | Core, json, tokenizer, ctype, filter | ~3 MB |
| `default` | + mbstring, pdo_sqlite, xml, bcmath | ~7.5 MB |
| `full` | + gd, zip, openssl, exif, fileinfo | ~14 MB |

## ccache

Build times with a warm ccache are significantly faster. The Dockerfile uses
a volume mount for ccache. For direct builds:

```bash
export CCACHE_DIR=/tmp/ccache-php-wasm
# Then run configure-php.sh / make as normal
```

## Troubleshooting

**`wasi-sdk not found at /opt/wasi-sdk`**
→ Set `WASI_SDK_PATH=/path/to/wasi-sdk` before running `build.sh`

**Patch fails with `error: patch failed`**
→ See `docs/UPDATING_PATCHES.md`

**`php-cgi` binary not found after make**
→ Check that `--enable-cgi` is in your `config.yaml` configure_extra

**`sha256 is a placeholder`**
→ Download the tarball manually and run `sha256sum` on it, then update `source.yaml`
