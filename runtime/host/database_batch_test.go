package runtimehost

import (
	"strings"
	"testing"
)

func TestNativeDatabaseBatches(t *testing.T) {
	out, _ := runPHPDatabase(t, `
function check($value, $name) { if (!$value) throw new RuntimeException($name); }
mysqli_report(MYSQLI_REPORT_OFF);
$db = new mysqli('localhost','root','','batch');
check((string)$db->query("SELECT LOWER('EMAIL') COLLATE utf8mb4_bin = LOWER('email') AS ok")->fetch_assoc()['ok'] === '1', 'email comparison');
check((string)$db->query("SELECT 'A' COLLATE utf8mb4_bin = 'a' AS ok")->fetch_assoc()['ok'] === '0', 'case-sensitive binary comparison');
check((string)$db->query("SELECT 'é ' COLLATE utf8mb4_bin = 'é' AS ok")->fetch_assoc()['ok'] === '1', 'binary PAD SPACE');
check(mysqli_multi_query($db, "SELECT 'a;b' AS v; /* ; */ SELECT 2 AS v; -- trailing comment") === true, 'batch result');
$result = $db->store_result(); check($result->fetch_assoc()['v'] === 'a;b', 'first row'); $result->close();
check(mysqli_more_results($db) && !$db->query('SELECT 1'), 'pending results');
check(mysqli_next_result($db), 'second result');
check((string)$db->store_result()->fetch_assoc()['v'] === '2', 'second row');
check(!$db->more_results() && !$db->next_result(), 'finished');
check(!$db->query('SELECT 1; SELECT 2'), 'ordinary query policy');
check(!$db->multi_query("SELECT 1; ATTACH DATABASE '/tmp/escape' AS other"), 'batch file policy');
check(!$db->multi_query("SELECT 1; /*! SELECT 2 */"), 'executable comment policy');
check(!$db->multi_query(str_repeat('SELECT 1;', 2049)), 'statement limit');
check(!$db->multi_query('SELECT '.str_repeat(' ',524288).'1'), 'byte limit');
check($db->multi_query('SELECT 1; SELECT * FROM nonexistent; SELECT 3'), 'initial result');
check(!$db->next_result() && !$db->more_results() && $db->errno !== 0, 'failed batch clears queue');
check($db->query('CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY)'), 'table');
check($db->query('SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED'), 'isolation');
check($db->begin_transaction() && $db->query('INSERT INTO rollback_probe VALUES (1)') && $db->rollback(), 'transaction');
check($db->query('SELECT * FROM rollback_probe')->num_rows === 0, 'rollback');
echo 'BATCH_OK';
`, []Config{{Version: 1}}, false, databaseWorker(t))
	if !strings.HasSuffix(strings.TrimSpace(out), "BATCH_OK") {
		t.Fatal(out)
	}
}
