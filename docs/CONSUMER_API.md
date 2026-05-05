# Consumer API

This document describes the stable contract between `php-wasm` WASM binaries
and their host runtimes (wazero, wasmtime, WasmEdge, etc.).

## WASM Module Imports

The php-wasm binary imports **only** `wasi_snapshot_preview1` functions.
There are no custom host imports. Any WASM runtime that implements
WASI Preview1 can run these binaries without modification.

Required WASI imports (subset actually used by php-cgi):

```
wasi_snapshot_preview1::fd_read
wasi_snapshot_preview1::fd_write
wasi_snapshot_preview1::fd_close
wasi_snapshot_preview1::fd_seek
wasi_snapshot_preview1::fd_prestat_get
wasi_snapshot_preview1::fd_prestat_dir_name
wasi_snapshot_preview1::path_open
wasi_snapshot_preview1::path_filestat_get
wasi_snapshot_preview1::path_readdir
wasi_snapshot_preview1::path_unlink_file
wasi_snapshot_preview1::environ_get
wasi_snapshot_preview1::environ_sizes_get
wasi_snapshot_preview1::args_get
wasi_snapshot_preview1::args_sizes_get
wasi_snapshot_preview1::clock_time_get
wasi_snapshot_preview1::random_get
wasi_snapshot_preview1::proc_exit
wasi_snapshot_preview1::poll_oneoff
```

## WASM Module Exports

| Export | Type | Description |
|--------|------|-------------|
| `_start` | function | WASI command entry point (CGI mode) |
| `memory` | memory | Linear memory (minimum 256 pages = 16 MB) |

## CGI Invocation Contract

php-wasm runs as a **WASI Command** (not a Reactor). Each instantiation
handles exactly one CGI request and exits.

### Required Environment Variables

| Variable | Example | Notes |
|----------|---------|-------|
| `REQUEST_METHOD` | `GET` or `POST` | Required; empty defaults to GET |
| `SCRIPT_FILENAME` | `/srv/app/index.php` | Absolute WASI path to the PHP script |
| `REDIRECT_STATUS` | `200` | Required for CGI; PHP checks this |
| `GATEWAY_INTERFACE` | `CGI/1.1` | Identifies as CGI |

### Optional Environment Variables

| Variable | Example | Notes |
|----------|---------|-------|
| `QUERY_STRING` | `foo=bar&baz=1` | URL query parameters |
| `CONTENT_LENGTH` | `42` | Body length for POST; leave empty for GET |
| `CONTENT_TYPE` | `application/json` | Content-Type of POST body |
| `HTTP_HOST` | `example.com` | Populates `$_SERVER['HTTP_HOST']` |
| `HTTP_COOKIE` | `session=abc` | Populates `$_COOKIE` |
| `SERVER_NAME` | `localhost` | Populates `$_SERVER['SERVER_NAME']` |
| `SERVER_PORT` | `80` | Populates `$_SERVER['SERVER_PORT']` |
| `PATH_INFO` | `/extra/path` | Extra path after script |
| `DOCUMENT_ROOT` | `/srv/app` | Must match the pre-opened dir |

### WASI-Specific Variables (set by build system)

| Variable | Example | Description |
|----------|---------|-------------|
| `PHP_WASM_TARGET` | `wasm32-wasi` | Always set; use to detect WASM context |
| `PHP_WASM_PROFILE` | `default` | Build profile name |

## Filesystem Pre-opens

The following directories must be pre-opened at instantiation time:

| Host path | WASI mount point | PHP sees |
|-----------|-----------------|----------|
| `<docroot>` | `/srv/app` | `$_SERVER['DOCUMENT_ROOT']` |
| `/tmp` (host) | `/tmp` | `sys_get_temp_dir()` = `/tmp` |
| `<ini-dir>` | `/etc/php` | `php.ini` scan directory |

The `/srv/app` pre-open must have **read** permission. Write permission is only
needed if PHP scripts write files (e.g. caches, uploads). `/tmp` must have
read+write.

## stdin / stdout / stderr

| Stream | Direction | Content |
|--------|-----------|---------|
| stdin (fd 0) | Host → WASM | POST request body (raw bytes) |
| stdout (fd 1) | WASM → Host | CGI response: headers + `\r\n\r\n` + body |
| stderr (fd 2) | WASM → Host | PHP error_log output, notices, warnings |

### CGI Response Format

stdout output is a CGI response. Split on the first `\r\n\r\n` (or `\n\n`):

```
Content-Type: text/html; charset=UTF-8\r\n
X-Powered-By: PHP/8.3.30-wasi\r\n
\r\n
<html>...body...</html>
```

The host must parse this split and forward body + headers to the HTTP client.

## Unsupported Features (wasm32-wasi Preview1)

| Feature | PHP function | Behavior |
|---------|-------------|---------|
| Process spawning | `exec()`, `system()`, `popen()` | Returns `false`, `E_WARNING` |
| Subprocesses | `proc_open()` | Returns `false`, `E_WARNING` |
| Fibers | `new Fiber()` / `Fiber::suspend()` | Creation OK; resume throws `E_ERROR` |
| Network sockets | `fsockopen()`, `socket_*` | Blocked; no network in WASI Preview1 |
| JIT / opcache | automatic | Disabled at build time |
| Shared memory | `shmop_*`, `apcu` | Not available |
| Signals | `pcntl_signal()` | Emulated via `libwasi-emulated-signal` |
| Threads | `pthreads`, `parallel` | Not available in WASM |

## Stability Guarantees (SemVer)

| Category | Guarantee |
|----------|-----------|
| WASI imports | Stable: always and only `wasi_snapshot_preview1` |
| `_start` export | Stable: always present in CGI builds |
| CGI env var names | Stable: follows CGI/1.1 RFC |
| PHP_WASM_TARGET | Stable: always `wasm32-wasi` |
| PHP_WASM_PROFILE | Stable: one of `minimal`, `default`, `full` |
| Binary size | Not guaranteed; may change with PHP upstream |
| Extension list | Follows profile YAML; changes are semver-bumped |

Artifact naming: `php-{php_version}-{profile}.wasm`
Example: `php-8.3.30-default.wasm`
