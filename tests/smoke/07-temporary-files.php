<?php
// Regression for the WASI temporary-file stubs that broke multipart uploads.
declare(strict_types=1);

$directory = sys_get_temp_dir();
if (!is_string($directory) || $directory === '' || !is_dir($directory)) {
    echo "FAIL: no usable temporary directory\n";
    exit(1);
}
$paths = [];
for ($i = 0; $i < 4; $i++) {
    $file = tempnam($directory, 'php-wasm-');
    if ($file === false || in_array($file, $paths, true) || !is_file($file)) {
        echo "FAIL: temporary files must exist and have unique names\n";
        exit(1);
    }
    $paths[] = $file;
    if (file_put_contents($file, 'temporary data') !== 14 || file_get_contents($file) !== 'temporary data') {
        echo "FAIL: temporary file read/write failed\n";
        exit(1);
    }
}
foreach ($paths as $file) {
    unlink($file);
}
$stream = tmpfile();
if ($stream === false || fwrite($stream, 'stream') !== 6 || !rewind($stream) || stream_get_contents($stream) !== 'stream') {
    echo "FAIL: temporary stream failed\n";
    exit(1);
}
fclose($stream);
echo "OK: 07-temporary-files\n";
