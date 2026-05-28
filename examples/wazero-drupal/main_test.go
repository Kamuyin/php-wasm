package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFastCGIRequestExecutesPHP(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "out", "php-*-drupal.wasm"))
	if err != nil {
		t.Fatalf("glob wasm: %v", err)
	}
	if len(matches) == 0 {
		t.Skip("no drupal wasm artifact found in ../../out")
	}

	docroot := t.TempDir()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(docroot, "index.php"), []byte("<?php echo 'fastcgi-ok';"), 0644); err != nil {
		t.Fatalf("write index.php: %v", err)
	}

	srv, err := newServer(matches[0], docroot, dataDir, "./php.ini", 10*time.Second)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer srv.close()

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://example.test/index.php", nil)
		req.Host = "example.test"
		rec := httptest.NewRecorder()

		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
		if body := strings.TrimSpace(rec.Body.String()); body != "fastcgi-ok" {
			t.Fatalf("request %d: unexpected body %q", i+1, body)
		}
	}
}
