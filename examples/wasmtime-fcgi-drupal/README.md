# Wasmtime-Go Persistent FastCGI Drupal Example

This example demonstrates how to run Drupal 10 over a **persistent** PHP WASM instance using `wasmtime-go` and the FastCGI binary protocol.

Instead of paying the overhead of compiling/instantiating the WASM module on every HTTP request, this Go server instantiates the `wasm32-wasi` PHP binary exactly once. It connects `stdin` and `stdout` using OS pipes (`/proc/self/fd/*`), and tunnels all incoming HTTP traffic into binary FastCGI streams over the pipes.

## 1. Setup

First, download Drupal, apply WASI compatibility patches, and generate the SQLite settings:

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
./server --wasm ../../out/php-8.3.30-drupal.wasm --addr :8080
```

*Note: You must have built the `drupal` profile of `php-wasm` first via `make` or `scripts/build.sh 8.3 drupal`.*

## 4. Install Drupal

Open your browser to:

[http://localhost:8080/core/install.php](http://localhost:8080/core/install.php)

The SQLite database will be initialized automatically inside the `data/database/` directory.
