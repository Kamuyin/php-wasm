<?php
// Smoke test 01: Basic PHP execution
declare(strict_types=1);

echo "Hello from PHP " . PHP_VERSION . " (WASI)\n";
echo "SAPI: " . php_sapi_name() . "\n";
echo "PHP_OS: " . PHP_OS . "\n";

// Verify WASM-specific server vars if set
if (isset($_SERVER['PHP_WASM_TARGET'])) {
    echo "WASM target: " . $_SERVER['PHP_WASM_TARGET'] . "\n";
}
if (isset($_SERVER['PHP_WASM_PROFILE'])) {
    echo "WASM profile: " . $_SERVER['PHP_WASM_PROFILE'] . "\n";
}

// Basic arithmetic
assert(1 + 1 === 2, "arithmetic check failed");
assert(PHP_INT_SIZE === 4, "wasm32 int size must be 4 bytes");

echo "OK: 01-hello\n";
