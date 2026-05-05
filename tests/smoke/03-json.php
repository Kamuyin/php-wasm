<?php
// Smoke test 03: JSON encode/decode
declare(strict_types=1);

if (!extension_loaded('json')) {
    fwrite(STDERR, "SKIP: json extension not loaded\n");
    exit(0);
}

// Encode
$data = [
    'name'    => 'php-wasm',
    'version' => PHP_VERSION,
    'target'  => 'wasm32-wasi',
    'numbers' => [1, 2, 3, PHP_INT_MAX],
    'nested'  => ['a' => true, 'b' => null, 'c' => 3.14],
    'unicode' => "Ünïcödé テスト",
];

$json = json_encode($data, JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
assert(is_string($json), "json_encode must return string");
assert(str_contains($json, 'wasm32-wasi'), "JSON must contain target");
echo "json_encode OK (" . strlen($json) . " bytes)\n";

// Round-trip
$decoded = json_decode($json, true, 512, JSON_THROW_ON_ERROR);
assert($decoded['name'] === 'php-wasm', "round-trip name mismatch");
assert($decoded['numbers'][3] === PHP_INT_MAX, "int64 round-trip failed");
assert($decoded['nested']['b'] === null, "null round-trip failed");
assert($decoded['unicode'] === "Ünïcödé テスト", "unicode round-trip failed");
echo "json_decode OK\n";

// Error handling
try {
    json_decode('{invalid}', true, 512, JSON_THROW_ON_ERROR);
    fwrite(STDERR, "FAIL: Should have thrown on invalid JSON\n");
    exit(1);
} catch (JsonException $e) {
    echo "JSON error handling OK: " . $e->getMessage() . "\n";
}

echo "OK: 03-json\n";
