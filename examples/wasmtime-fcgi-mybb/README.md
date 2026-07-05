# Wasmtime-Go Persistent FastCGI MyBB Example

This example demonstrates how to run MyBB over a **persistent** PHP WASM instance using `wasmtime-go` and the FastCGI binary protocol.

Instead of paying the overhead of compiling/instantiating the WASM module on every HTTP request, this Go server instantiates the `wasm32-wasi` PHP binary exactly once. It connects `stdin` and `stdout` using OS pipes (`/proc/self/fd/*`), and tunnels all incoming HTTP traffic into binary FastCGI streams over the pipes.

## 1. Setup

First, download MyBB and configure the files for the installer:

```bash
./setup.sh
```

## 2. Build the server

```bash
go build -o server .
```

## 3. Run the Server

Start the persistent server, pointing it to the compiled WASM binary (this example reuses the `wordpress` profile binary since it includes SQLite and all extensions required by MyBB):

```bash
./server --wasm ../../out/php-8.3.30-wordpress.wasm --addr :8080
```

*Note: You must have built the `wordpress` profile of `php-wasm` first via `make` or `scripts/build.sh 8.3 wordpress`.*

## 4. Install MyBB

Open your browser to:

[http://localhost:8080/install/](http://localhost:8080/install/)

When prompted for **Database Configuration**:
- **Database Engine**: Choose `SQLite 3`
- **Database Path**: Enter `/data/database/mybb.sqlite`

The database will be stored safely inside the `data/database/` directory, persisting across server restarts.

## Architecture

1. **`main.go`**: Starts a Go `http.Server`. Compiles the WASM module. Sets up a background goroutine to execute `_start` on the WASM module with `PHP_FCGI_FORCE=1`.
2. **`fcgi.go`**: A lightweight, standalone FastCGI encoder/decoder. Translates `*http.Request` to `FCGI_BEGIN_REQUEST`/`FCGI_PARAMS`/`FCGI_STDIN` packets, writes them to the persistent pipe, and decodes the response.
