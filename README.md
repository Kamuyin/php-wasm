# php-wasm

[![Build Status](https://github.com/your-org/php-wasm/actions/workflows/build.yml/badge.svg)](https://github.com/your-org/php-wasm/actions/workflows/build.yml)
[![Latest PHP](https://img.shields.io/badge/PHP-8.4.20--wasi-blue)](https://github.com/your-org/php-wasm/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![WASI: Preview1](https://img.shields.io/badge/WASI-Preview1-green)](https://github.com/WebAssembly/WASI)

**php-wasm** is a build system that compiles PHP to [WebAssembly](https://webassembly.org/) targeting
[WASI Preview1](https://wasi.dev/). It produces `wasm32-wasi` binaries that run in any
WASI-compatible runtime — including [wazero](https://wazero.io/), wasmtime, and WasmEdge —
**without JavaScript glue code** and **without Emscripten**.

This repo contains **patches and build infrastructure only**. PHP source code is fetched
from the official [php/php-src](https://github.com/php/php-src) repository at build time.

## Quickstart

```bash
# Build PHP 8.3 (default profile) in Docker
make docker-build VERSION=8.3 PROFILE=default

# Run smoke tests
cd tests/runners/wazero && go build -o php-wasm-runner .
./tests/smoke/run-smoke.sh --wasm ../../out/php-8.3.30-default.wasm

# Serve PHP scripts over HTTP
cd examples/wazero-cgi-minimal
go run main.go --wasm ../../out/php-8.3.30-default.wasm --docroot ./www --addr :8080
```

## Supported Versions

| PHP Version | Status | EOL |
|-------------|--------|-----|
| 8.4 | Active | 2028-12-31 |
| 8.3 | Active (primary) | 2027-12-31 |
| 8.2 | Security only | 2026-12-31 |

## Build Profiles

| Profile | Extensions | Size |
|---------|-----------|------|
| `minimal` | Core, json, tokenizer, ctype, filter | ~3 MB |
| `default` | + mbstring, pdo_sqlite, bcmath | ~7.5 MB |
| `wordpress` | + xml, dom, simplexml, fileinfo, exif | ~7–8 MB |
| `drupal` | Similar to wordpress | ~8 MB |
| `full` | + gd, zip, openssl, exif, fileinfo | ~14 MB |

## Consumer Example (wazero / Go)

```go
package main

import (
    "context"
    "os"

    "github.com/tetratelabs/wazero"
    "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func main() {
    wasmBytes, _ := os.ReadFile("php-8.3.30-default.wasm")

    ctx := context.Background()
    rt  := wazero.NewRuntime(ctx)
    defer rt.Close(ctx)

    wasi_snapshot_preview1.MustInstantiate(ctx, rt)

    compiled, _ := rt.CompileModule(ctx, wasmBytes)
    defer compiled.Close(ctx)

    // one module instance per CGI request
    mod, _ := rt.InstantiateModule(ctx, compiled,
        wazero.NewModuleConfig().
            WithEnv("REQUEST_METHOD", "GET").
            WithEnv("SCRIPT_FILENAME", "/srv/app/index.php").
            WithEnv("REDIRECT_STATUS", "200").
            WithFSConfig(wazero.NewFSConfig().
                WithDirMount("./www", "/srv/app").
                WithDirMount("/tmp", "/tmp")))
    mod.Close(ctx)
}
```

See `examples/wazero-cgi-minimal/` for a complete HTTP server example.

## Limitations (wasm32-wasi Preview1)

| Feature | Status |
|---------|--------|
| Networking (TCP/UDP) | Not available — WASI Preview1 has no network socket API |
| Fibers / coroutines | Not available — no ucontext in WASM |
| fork / exec / popen | Stubbed — returns `false` + `E_WARNING` |
| opcache / JIT | Disabled — no RWX memory pages in WASM |
| Threads | Not available |
| MySQL / PostgreSQL | Not available — requires network sockets |
| PCRE JIT | Disabled |
| `mail()` | Disabled |

## Documentation

- [Building locally](docs/BUILDING.md)
- [Architecture overview](docs/ARCHITECTURE.md)
- [Consumer API contract](docs/CONSUMER_API.md)
- [Adding a PHP version](docs/ADDING_VERSION.md)
- [Updating patches](docs/UPDATING_PATCHES.md)
- [WordPress guide](docs/WORDPRESS.md)
- [Drupal guide](docs/DRUPAL.md)

## License

MIT — see [LICENSE](LICENSE).

PHP is licensed under the [PHP License](https://www.php.net/license/). This repository
does not include PHP source code; it is fetched at build time.
