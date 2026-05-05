<?php
// Smoke test 04: PDO + SQLite
declare(strict_types=1);

if (!extension_loaded('pdo') || !extension_loaded('pdo_sqlite')) {
    echo "SKIP: pdo or pdo_sqlite extension not loaded\n";
    exit(0);
}

// In-memory SQLite database
$pdo = new PDO('sqlite::memory:', null, null, [
    PDO::ATTR_ERRMODE            => PDO::ERRMODE_EXCEPTION,
    PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
]);

echo "SQLite version: " . $pdo->query("SELECT sqlite_version()")->fetchColumn() . "\n";

// Create table
$pdo->exec("CREATE TABLE users (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    age  INTEGER NOT NULL
)");

// Insert with prepared statement (prevents SQL injection)
$stmt = $pdo->prepare("INSERT INTO users (name, age) VALUES (:name, :age)");
$users = [
    ['Alice', 30],
    ['Bob', 25],
    ['Carol', 35],
    ["O'Brien", 40],  // name with single quote — tests escaping
];
foreach ($users as [$name, $age]) {
    $stmt->execute([':name' => $name, ':age' => $age]);
}

// Query
$rows = $pdo->query("SELECT * FROM users ORDER BY age ASC")->fetchAll();
assert(count($rows) === 4, "Expected 4 rows, got " . count($rows));
assert($rows[0]['name'] === 'Bob', "First row (youngest) should be Bob");
assert($rows[3]['name'] === "O'Brien", "Quoted name round-trip failed");
echo "INSERT + SELECT OK (" . count($rows) . " rows)\n";

// Aggregate
$avg = $pdo->query("SELECT AVG(age) FROM users")->fetchColumn();
assert(abs($avg - 32.5) < 0.001, "AVG age mismatch: $avg");
echo "Aggregate OK (avg age: $avg)\n";

// Transaction
$pdo->beginTransaction();
$pdo->exec("DELETE FROM users WHERE age < 30");
$count = $pdo->query("SELECT COUNT(*) FROM users")->fetchColumn();
assert((int)$count === 3, "After delete, expected 3 rows");
$pdo->rollBack();
$count = $pdo->query("SELECT COUNT(*) FROM users")->fetchColumn();
assert((int)$count === 4, "After rollback, expected 4 rows");
echo "Transaction + rollback OK\n";

echo "OK: 04-sqlite\n";
