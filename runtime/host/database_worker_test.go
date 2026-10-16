package runtimehost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func TestEmbeddedDatabaseWorker(t *testing.T) {
	binaryPath, backend := os.Getenv("PHP_WASM_ORDINARY_BINARY"), os.Getenv("PHP_WASM_DATABASE_BACKEND")
	if binaryPath == "" || backend == "" {
		t.Skip("set ordinary PHP binary and prepared database backend directory")
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	query := func(connection int, sql string, params ...any) map[string]any {
		return map[string]any{"op": "query", "connection": connection, "sql_b64": base64.StdEncoding.EncodeToString([]byte(sql)), "params": params}
	}
	requests := []map[string]any{
		{"op": "connect", "driver": "mysql", "database": "wordpress"},
		query(1, "CREATE TABLE wp_options (option_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, option_name VARCHAR(191) NOT NULL DEFAULT '', option_value LONGTEXT NOT NULL, autoload VARCHAR(20) NOT NULL DEFAULT 'yes', PRIMARY KEY (option_id), UNIQUE KEY option_name (option_name)) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"),
		query(1, "INSERT INTO wp_options (option_name,option_value) VALUES (?,?)", "siteurl", "http://example.test"),
		query(1, "SELECT option_value FROM wp_options WHERE option_name = 'siteurl'"),
		query(1, "SHOW TABLES"),
		query(1, "SELECT VERSION()"),
		query(1, "ATTACH '/tmp/escape.sqlite' AS external"),
		{"op": "connect", "driver": "pgsql", "database": "app"},
		query(2, "CREATE TABLE records (id SERIAL PRIMARY KEY, value TEXT NOT NULL)"),
		query(2, "INSERT INTO records (value) VALUES ($1) RETURNING id", "bound ' value"),
		query(2, "SELECT value FROM records WHERE id = $1", 1),
		query(2, "BEGIN"), query(2, "INSERT INTO records (value) VALUES ($1)", "rollback"), query(2, "ROLLBACK"),
		query(2, "SELECT COUNT(*) FROM records"),
		{"op": "reset"},
		{"op": "connect", "driver": "mysql", "database": "wordpress"},
		query(1, "SELECT option_value FROM wp_options WHERE option_name = 'siteurl'"),
	}
	requests = append(requests, map[string]any{"op": "connect", "driver": "pgsql", "database": "app"})
	for _, sql := range []string{"SELECT * FROM 'sqlite_master'", "SELECT * FROM pragma_database_list", "SELECT * FROM main.'sqlite_master'", "SELECT load_extension('/tmp/x')"} {
		requests = append(requests, query(2, sql))
	}
	for _, sql := range []string{`SET sql_mode='NO_BACKSLASH_ESCAPES'`, `SET sql_mode='NO_BACKSLASH_\ESCAPES'`, `SET sql_mode=CONCAT('NO_BACKSLASH_','ESCAPES')`, `SET sql_mode='ANSI_QUOTES'`} {
		requests = append(requests, query(1, sql))
	}
	var stdin, stdout, stderr bytes.Buffer
	for _, request := range requests {
		data, _ := json.Marshal(request)
		stdin.Write(data)
		stdin.WriteByte('\n')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(4096))
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	compiled, err := rt.CompileModule(ctx, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer compiled.Close(ctx)
	_, err = rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithArgs("php-cgi", "-q", "-n", "-d", "memory_limit=192M", "-d", "display_errors=1", "-f", "/backend/worker.php").WithEnv("USE_ZEND_ALLOC", "0").WithEnv("REDIRECT_STATUS", "200").WithStdin(&stdin).WithStdout(&stdout).WithStderr(&stderr).WithFSConfig(wazero.NewFSConfig().WithReadOnlyDirMount(backend, "/backend").WithDirMount(t.TempDir(), "/data").WithDirMount(t.TempDir(), "/tmp")))
	var exit *sys.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 0) {
		t.Fatalf("worker: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(requests)+1 || lines[0] != `{"ready":1}` {
		t.Fatalf("worker framing: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	for i, line := range lines[1:] {
		var result map[string]any
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			t.Fatal(err)
		}
		if (result["ok"] == true) != (i != 6 && i < 19) {
			t.Errorf("request %d %v -> %s; stderr: %s", i, requests[i], line, stderr.String())
		}
		if i == 3 || i == 17 {
			rows, _ := result["rows"].([]any)
			if len(rows) != 1 {
				t.Errorf("missing persisted result: %s", line)
			}
		}
		if i == 14 && !strings.Contains(line, `[[1]]`) {
			t.Errorf("rollback did not retain exactly one row: %s", line)
		}
	}
}
