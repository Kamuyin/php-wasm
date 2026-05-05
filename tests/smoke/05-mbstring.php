<?php
// Smoke test 05: mbstring multibyte string functions
declare(strict_types=1);

if (!extension_loaded('mbstring')) {
    fwrite(STDERR, "SKIP: mbstring extension not loaded\n");
    exit(0);
}

// Character length vs byte length
$str = "Héllo Wörld";
assert(mb_strlen($str, 'UTF-8') === 11, "mb_strlen failed: " . mb_strlen($str));
assert(strlen($str) > 11, "strlen must be > mb_strlen for multibyte string");
echo "mb_strlen OK: " . mb_strlen($str) . " chars\n";

// Substr
$sub = mb_substr($str, 0, 5, 'UTF-8');
assert($sub === "Héllo", "mb_substr failed: $sub");
echo "mb_substr OK: $sub\n";

// strtoupper / strtolower
$upper = mb_strtoupper("héllo wörld", 'UTF-8');
assert($upper === "HÉLLO WÖRLD", "mb_strtoupper failed: $upper");
$lower = mb_strtolower("HÉLLO WÖRLD", 'UTF-8');
assert($lower === "héllo wörld", "mb_strtolower failed: $lower");
echo "mb_strtoupper / mb_strtolower OK\n";

// Japanese multi-byte
$ja = "日本語テスト";
assert(mb_strlen($ja, 'UTF-8') === 6, "Japanese strlen failed");
assert(mb_substr($ja, 0, 3, 'UTF-8') === "日本語", "Japanese substr failed");
echo "Japanese multibyte OK\n";

// Encoding detection
$encoding = mb_detect_encoding("Hello", ['ASCII', 'UTF-8'], true);
assert($encoding === 'ASCII' || $encoding === 'UTF-8', "encoding detection failed: $encoding");
echo "mb_detect_encoding OK: $encoding\n";

// convert_encoding round-trip
$utf8   = "Héllo";
$latin1 = mb_convert_encoding($utf8, 'ISO-8859-1', 'UTF-8');
$back   = mb_convert_encoding($latin1, 'UTF-8', 'ISO-8859-1');
assert($back === $utf8, "convert_encoding round-trip failed: $back");
echo "convert_encoding OK\n";

echo "OK: 05-mbstring\n";
