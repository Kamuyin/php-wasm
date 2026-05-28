<?php
// Smoke test 06: libxml2-backed XML stack (xml, dom, simplexml).
// Validates that the wordpress profile's XML extensions link and round-trip
// data correctly. Skipped on minimal/default profiles.
declare(strict_types=1);

if (!extension_loaded('dom') || !extension_loaded('simplexml') || !extension_loaded('xml')) {
    echo "SKIP: dom, simplexml, or xml extension not loaded\n";
    exit(0);
}

$xml = <<<'XML'
<?xml version="1.0" encoding="UTF-8"?>
<feed>
    <title>Hello WASI</title>
    <entry id="1"><name>Alice</name></entry>
    <entry id="2"><name>O'Brien</name></entry>
</feed>
XML;

// --- DOMDocument ---
$dom = new DOMDocument();
$dom->loadXML($xml);
$entries = $dom->getElementsByTagName('entry');
assert($entries->length === 2, "DOM: expected 2 entries, got {$entries->length}");
$first = $entries->item(0);
assert($first->getAttribute('id') === '1', "DOM: first entry id mismatch");
echo "DOMDocument OK ({$entries->length} entries)\n";

// --- SimpleXML ---
$sx = simplexml_load_string($xml);
assert((string)$sx->title === 'Hello WASI', "SimpleXML: title mismatch");
assert(count($sx->entry) === 2, "SimpleXML: expected 2 entries");
assert((string)$sx->entry[1]->name === "O'Brien", "SimpleXML: quoted text round-trip failed");
echo "SimpleXML OK (title: {$sx->title})\n";

// --- XMLReader (streaming) ---
if (extension_loaded('xmlreader')) {
    $reader = new XMLReader();
    $reader->XML($xml);
    $entryCount = 0;
    while ($reader->read()) {
        if ($reader->nodeType === XMLReader::ELEMENT && $reader->name === 'entry') {
            $entryCount++;
        }
    }
    $reader->close();
    assert($entryCount === 2, "XMLReader: expected 2 entries, got {$entryCount}");
    echo "XMLReader OK ({$entryCount} entries)\n";
}

// --- XMLWriter (output) ---
if (extension_loaded('xmlwriter')) {
    $writer = new XMLWriter();
    $writer->openMemory();
    $writer->startDocument('1.0', 'UTF-8');
    $writer->startElement('greeting');
    $writer->text('hello');
    $writer->endElement();
    $writer->endDocument();
    $output = $writer->outputMemory();
    assert(strpos($output, '<greeting>hello</greeting>') !== false, "XMLWriter: output mismatch");
    echo "XMLWriter OK\n";
}

echo "OK: 06-xml\n";
