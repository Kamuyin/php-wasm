# Wasmtime-Go Persistent FastCGI WordPress Example

This example demonstrates how to run WordPress over a **persistent** PHP WASM instance using `wasmtime-go` and the FastCGI binary protocol.

Instead of paying the overhead of compiling/instantiating the WASM module on every HTTP request, this Go server instantiates the `wasm32-wasi` PHP binary exactly once. It connects `stdin` and `stdout` using OS pipes (`/proc/self/fd/*`), and tunnels all incoming HTTP traffic into binary FastCGI streams over the pipes.

This provides the exact same request lifecycle architecture found in production platforms like the Gimpel runtime.

## 1. Setup

First, download WordPress, the SQLite integration plugin, and configure the files:

```bash
./setup.sh
```

## 2. Build the server

```bash
go build -o server .
```

## 3. Run the Server

Start the persistent server, pointing it to the compiled WASM binary:

```bash
./server --wasm ../../out/php-8.3.30-wordpress.wasm --addr :8080
```

*Note: You must have built the `wordpress` profile of `php-wasm` first via `make` or `scripts/build.sh 8.3 wordpress`.*

## 4. Install WordPress

Open your browser to:

[http://localhost:8080/wp-admin/install.php](http://localhost:8080/wp-admin/install.php)

The SQLite database will be initialized automatically inside the `data/database/` directory.

## Architecture

1. **`main.go`**: Starts a Go `http.Server`. Compiles the WASM module. Sets up a background goroutine to execute `_start` on the WASM module with `PHP_FCGI_FORCE=1`.
2. **`fcgi.go`**: A lightweight, standalone FastCGI encoder/decoder. Translates `*http.Request` to `FCGI_BEGIN_REQUEST`/`FCGI_PARAMS`/`FCGI_STDIN` packets, writes them to the persistent pipe, and decodes the response.
