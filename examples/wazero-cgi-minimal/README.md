# wazero-cgi-minimal

Minimal HTTP server that serves PHP scripts via a `php-cgi` WASM binary.

## Requirements

- Go 1.22+
- A `php-cgi` WASM binary built with this repo (e.g. `out/php-8.3.30-default.wasm`)

## Quick start

```bash
# Build the example
go build -o php-server .

# Create a document root with a test script
mkdir www
cat > www/index.php <<'EOF'
<?php
header('Content-Type: text/plain');
echo "PHP " . PHP_VERSION . " running under " . php_sapi_name() . "\n";
echo "Target: " . ($_SERVER['PHP_WASM_TARGET'] ?? 'native') . "\n";
phpinfo(INFO_GENERAL);
EOF

# Run the server (pointing at a built .wasm binary)
./php-server \
  --wasm ../../out/php-8.3.30-default.wasm \
  --docroot ./www \
  --addr :8080

# Test it
curl http://localhost:8080/index.php
```

## Architecture

```
HTTP request
    |
    v
net/http handler
    |
    +-- resolves SCRIPT_FILENAME from URL + docroot
    +-- builds CGI/1.1 environment variables
    +-- reads request body into bytes.Buffer
    |
    v
wazero.InstantiateModule (fresh per request)
    |
    +-- WASI stdin  = request body
    +-- WASI stdout = CGI response (headers + body)
    +-- WASI stderr = PHP errors (logged, not forwarded)
    +-- WASI preopens: docroot -> /srv/app, /tmp -> /tmp
    |
    v
CGI response parser (splits at \r\n\r\n)
    |
    v
HTTP response writer
```

## Request isolation

Each request gets a **fresh `wazero.ModuleInstance`**. This means:

- PHP global state (`$_SERVER`, `$_GET`, etc.) is reset per request
- No memory leaks across requests
- No shared globals between concurrent requests

The **compiled module** (`wazero.CompiledModule`) is shared and reused,
so WASM compilation cost (typically 1-3 seconds for php.wasm) is paid
once at startup.

## Per-request timeout

The server uses `context.WithTimeout(10s)` as a watchdog. If PHP takes
longer than 10 seconds, the request returns `504 Gateway Timeout` and
the WASM module is killed.

## CGI environment mapping

| HTTP concept | CGI variable |
|---|---|
| `r.Method` | `REQUEST_METHOD` |
| `r.URL.RawQuery` | `QUERY_STRING` |
| `r.Header["Content-Type"]` | `CONTENT_TYPE` |
| Body length | `CONTENT_LENGTH` |
| Script path | `SCRIPT_FILENAME` (absolute path in container) |
| `r.RemoteAddr` | `REMOTE_ADDR` + `REMOTE_PORT` |
| `X-Request-ID` generated | `HTTP_X_REQUEST_ID` (available in `$_SERVER`) |

## WASM filesystem layout

| Host path | WASI path | PHP sees |
|---|---|---|
| `--docroot` | `/srv/app` | `DOCUMENT_ROOT=/srv/app` |
| `/tmp` | `/tmp` | `sys_get_temp_dir() == /tmp` |
| `--php-ini dir` | `/etc/php` | `PHP_INI_SCAN_DIR=/etc/php` |

## Limitations

- No persistent connections (php-fpm/FastCGI not supported)
- No WASM threads (Fiber, parallel extensions)
- No network syscalls from PHP (MySQL, Redis, etc.)
- No `exec()`, `system()`, `popen()` (stubbed with ENOSYS)
