<?php
/**
 * WASI compatibility stubs for Drupal on wasm32-wasi / wazero.
 *
 * WASI Preview1 has no chmod/chown/lchown/chgrp syscalls, so the PHP WASM
 * build omits those functions from the function table entirely. When
 * namespaced Drupal code calls chmod(), PHP first looks in the current
 * namespace, then falls back to the global namespace. Defining them here
 * (via auto_prepend_file) satisfies that fallback with a no-op stub that
 * signals success, which is correct: on a WASM sandbox all files are
 * accessible to the single-tenant PHP process regardless of permission bits.
 */

if (!function_exists('chmod')) {
    function chmod(string $filename, int $permissions): bool { return true; }
}
if (!function_exists('chown')) {
    function chown(string $filename, string|int $user): bool { return true; }
}
if (!function_exists('lchown')) {
    function lchown(string $filename, string|int $user): bool { return true; }
}
if (!function_exists('chgrp')) {
    function chgrp(string $filename, string|int $group): bool { return true; }
}
if (!function_exists('lchgrp')) {
    function lchgrp(string $filename, string|int $group): bool { return true; }
}
