package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

//go:embed static/index.html
var indexHTML []byte

// phpRuntime holds a compiled PHP WASM module for one build profile.
type phpRuntime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	wasmPath string
}

type playground struct {
	runtimes map[string]*phpRuntime
	timeout  time.Duration
}

type runRequest struct {
	Code    string `json:"code"`
	Profile string `json:"profile"`
}

type runResponse struct {
	Output    string `json:"output"`
	Error     string `json:"error,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

func main() {
	wasmDir := flag.String("wasm-dir", "../../out", "Directory containing php-*.wasm binaries")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	timeout := flag.Duration("timeout", 15*time.Second, "Per-request PHP execution timeout")
	flag.Parse()

	ctx := context.Background()
	runtimes := make(map[string]*phpRuntime)

	for _, profile := range []string{"minimal", "default", "full"} {
		pattern := filepath.Join(*wasmDir, fmt.Sprintf("php-*-%s.wasm", profile))
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			log.Printf("profile %q: no .wasm found (%s) — skipping", profile, pattern)
			continue
		}
		wasmPath := matches[len(matches)-1]

		wasmBytes, err := os.ReadFile(wasmPath)
		if err != nil {
			log.Printf("profile %q: read %s: %v", profile, wasmPath, err)
			continue
		}

		cache := wazero.NewCompilationCache()
		rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
		wasi_snapshot_preview1.MustInstantiate(ctx, rt)

		log.Printf("profile %q: compiling %s ...", profile, filepath.Base(wasmPath))
		compiled, err := rt.CompileModule(ctx, wasmBytes)
		if err != nil {
			rt.Close(ctx)
			log.Printf("profile %q: compile error: %v", profile, err)
			continue
		}

		log.Printf("profile %q: ready", profile)
		runtimes[profile] = &phpRuntime{rt: rt, compiled: compiled, wasmPath: wasmPath}
	}

	if len(runtimes) == 0 {
		log.Fatal("No WASM profiles loaded. Build one first:\n  make build VERSION=8.3 PROFILE=minimal")
	}

	pg := &playground{runtimes: runtimes, timeout: *timeout}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/api/run", pg.handleRun)
	mux.HandleFunc("/api/profiles", pg.handleProfiles)

	available := sortedKeys(runtimes)
	log.Printf("Playground ready: http://localhost%s  (profiles: %v)", *addr, available)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (pg *playground) handleProfiles(w http.ResponseWriter, r *http.Request) {
	available := sortedKeys(pg.runtimes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(available)
}

func (pg *playground) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if len(req.Code) > 64*1024 {
		http.Error(w, "Code too large (max 64 KB)", http.StatusBadRequest)
		return
	}
	if req.Profile == "" {
		req.Profile = "minimal"
	}

	rt, ok := pg.runtimes[req.Profile]
	if !ok {
		http.Error(w, fmt.Sprintf("Profile %q not available", req.Profile), http.StatusBadRequest)
		return
	}

	start := time.Now()
	output, runErr := pg.execPHP(r.Context(), rt, req.Code)
	elapsed := time.Since(start)

	resp := runResponse{Output: output, ElapsedMs: elapsed.Milliseconds()}
	if runErr != nil {
		resp.Error = runErr.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (pg *playground) execPHP(parentCtx context.Context, rt *phpRuntime, code string) (string, error) {
	// Ensure code starts with a PHP open tag so the CGI SAPI parses it.
	if !strings.HasPrefix(strings.TrimSpace(code), "<?") {
		code = "<?php\n" + code
	}

	tmpDir, err := os.MkdirTemp("", "php-pg-*")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "script.php"), []byte(code), 0644); err != nil {
		return "", fmt.Errorf("write script: %w", err)
	}
	// Provide a writable /tmp inside the WASI sandbox.
	if err := os.MkdirAll(filepath.Join(tmpDir, "tmp"), 0755); err != nil {
		return "", fmt.Errorf("mkdir tmp: %w", err)
	}

	ctx, cancel := context.WithTimeout(parentCtx, pg.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	id := fmt.Sprintf("%d", time.Now().UnixNano())

	// Mount tmpDir at "/" so the script is visible as /script.php.
	fsConfig := wazero.NewFSConfig().
		WithDirMount(tmpDir, "/").
		WithDirMount(filepath.Join(tmpDir, "tmp"), "/tmp")

	modConfig := wazero.NewModuleConfig().
		WithName("php-cgi-"+id).
		WithStdout(&stdout).
		WithStderr(&stderr).
		WithStdin(strings.NewReader("")).
		WithFSConfig(fsConfig).
		WithArgs("php-cgi").
		WithEnv("REQUEST_METHOD", "GET").
		WithEnv("SCRIPT_FILENAME", "/script.php").
		WithEnv("SCRIPT_NAME", "/script.php").
		WithEnv("DOCUMENT_ROOT", "/").
		WithEnv("REDIRECT_STATUS", "200").
		WithEnv("QUERY_STRING", "").
		WithEnv("CONTENT_TYPE", "").
		WithEnv("CONTENT_LENGTH", "0").
		WithEnv("SERVER_NAME", "playground").
		WithEnv("SERVER_PORT", "80").
		WithEnv("GATEWAY_INTERFACE", "CGI/1.1").
		WithEnv("TMPDIR", "/tmp").
		WithEnv("TMP", "/tmp").
		WithEnv("TEMP", "/tmp")

	mod, err := rt.rt.InstantiateModule(ctx, rt.compiled, modConfig)
	if mod != nil {
		mod.Close(ctx)
	}

	// A zero-exit ExitError is normal for CGI — not a real error.
	if err != nil {
		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 0 {
			err = nil
		} else if ctx.Err() != nil {
			out := assemblePHPOutput(stdout.String(), stderr.String())
			return out, fmt.Errorf("execution timed out after %s", pg.timeout)
		}
	}

	return assemblePHPOutput(stdout.String(), stderr.String()), err
}

// assemblePHPOutput strips CGI headers from stdout and appends any stderr.
func assemblePHPOutput(raw, stderrOut string) string {
	body := raw
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		body = raw[i+4:]
	} else if i := strings.Index(raw, "\n\n"); i >= 0 {
		body = raw[i+2:]
	}
	if stderrOut != "" {
		body += "\n\n-- PHP Notice / Warning / Error --\n" + stderrOut
	}
	return body
}

func sortedKeys(m map[string]*phpRuntime) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
