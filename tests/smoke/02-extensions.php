<?php
// Smoke test 02: Verify loaded extensions snapshot
declare(strict_types=1);

$loaded = get_loaded_extensions();
sort($loaded);

echo "Loaded extensions (" . count($loaded) . "):\n";
foreach ($loaded as $ext) {
    echo "  - $ext\n";
}

// The profile is available via $_SERVER['PHP_WASM_PROFILE'] when built with 0099 patch
$profile = $_SERVER['PHP_WASM_PROFILE'] ?? getenv('PHP_WASM_PROFILE') ?: 'unknown';

$required_by_profile = [
    'minimal' => ['Core', 'json', 'tokenizer', 'ctype', 'filter'],
    'default' => ['Core', 'json', 'tokenizer', 'ctype', 'filter', 'mbstring',
                  'pdo', 'pdo_sqlite', 'sqlite3', 'xml', 'dom', 'simplexml'],
    'full'    => ['Core', 'json', 'tokenizer', 'ctype', 'filter', 'mbstring',
                  'pdo', 'pdo_sqlite', 'sqlite3', 'xml', 'dom', 'simplexml',
                  'gd', 'zip', 'exif', 'fileinfo'],
];

if (isset($required_by_profile[$profile])) {
    $missing = array_diff($required_by_profile[$profile], $loaded);
    if ($missing) {
        fwrite(STDERR, "FAIL: Missing extensions for profile '{$profile}': " . implode(', ', $missing) . "\n");
        exit(1);
    }
    echo "OK: All required extensions for profile '{$profile}' are loaded\n";
} else {
    echo "NOTE: Unknown profile '{$profile}', skipping extension check\n";
}

echo "OK: 02-extensions\n";
