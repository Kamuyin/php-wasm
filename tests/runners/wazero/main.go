// php-wasm-runner: minimal CGI runner for php-cgi WASM binaries using wazero.
//
// Usage:
//
//	go run main.go --wasm php.wasm --script test.php [--method GET] [--query-string "foo=bar"]
//	go run main.go --wasm php.wasm --script test.php --method POST --body '{"key":"val"}'
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() {
	wasmPath := flag.String("wasm", "", "Path to php-cgi .wasm binary (required)")
	scriptPath := flag.String("script", "", "Path to .php script to execute (required)")
	phpIni := flag.String("php-ini", "", "Path to php.ini (optional)")
	method := flag.String("method", "GET", "HTTP method (GET or POST)")
	queryString := flag.String("query-string", "", "CGI QUERY_STRING value")
	body := flag.String("body", "", "Request body for POST requests")
	timeout := flag.Duration("timeout", 30*time.Second, "Execution timeout")
	verbose := flag.Bool("verbose", false, "Print CGI environment and response headers")
	flag.Parse()

	if *wasmPath == "" || *scriptPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	code, err := run(runConfig{
		wasmPath:    *wasmPath,
		scriptPath:  *scriptPath,
		phpIni:      *phpIni,
		method:      strings.ToUpper(*method),
		queryString: *queryString,
		body:        *body,
		timeout:     *timeout,
		verbose:     *verbose,
	})
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	os.Exit(code)
}

type runConfig struct {
	wasmPath    string
	scriptPath  string
	phpIni      string
	method      string
	queryString string
	body        string
	timeout     time.Duration
	verbose     bool
}

func run(cfg runConfig) (exitCode int, err error) {
	wasmBytes, err := os.ReadFile(cfg.wasmPath)
	if err != nil {
		return 1, fmt.Errorf("reading wasm: %w", err)
	}

	scriptAbs, err := filepath.Abs(cfg.scriptPath)
	if err != nil {
		return 1, fmt.Errorf("resolving script path: %w", err)
	}

	scriptDir := filepath.Dir(scriptAbs)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	// Create wazero runtime with compilation cache
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(wazero.NewCompilationCache()))
	defer rt.Close(ctx)

	// Instantiate WASI
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	// Compile the PHP WASM module
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return 1, fmt.Errorf("compiling wasm module: %w", err)
	}
	defer compiled.Close(ctx)

	// Build CGI environment
	contentLength := fmt.Sprintf("%d", len(cfg.body))
	contentType := ""
	if cfg.method == "POST" {
		contentType = "application/x-www-form-urlencoded"
		if strings.HasPrefix(cfg.body, "{") || strings.HasPrefix(cfg.body, "[") {
			contentType = "application/json"
		}
	}

	// Inside the WASI sandbox, scriptDir is mounted at "/", so the script
	// is accessible as "/" + basename, not by its host absolute path.
	wasiScript := "/" + filepath.Base(scriptAbs)

	cgiEnv := []string{
		"REQUEST_METHOD=" + cfg.method,
		"QUERY_STRING=" + cfg.queryString,
		"SCRIPT_FILENAME=" + wasiScript,
		"SCRIPT_NAME=" + wasiScript,
		"PATH_INFO=",
		"PATH_TRANSLATED=" + wasiScript,
		"DOCUMENT_ROOT=/",
		"SERVER_NAME=localhost",
		"SERVER_PORT=80",
		"SERVER_PROTOCOL=HTTP/1.1",
		"HTTP_HOST=localhost",
		"GATEWAY_INTERFACE=CGI/1.1",
		"REDIRECT_STATUS=200",
		"CONTENT_LENGTH=" + contentLength,
		"CONTENT_TYPE=" + contentType,
		// PHP-specific
		"PHP_SELF=" + wasiScript,
	}

	if cfg.phpIni != "" {
		cgiEnv = append(cgiEnv, "PHP_INI_SCAN_DIR="+filepath.Dir(cfg.phpIni))
	}

	if cfg.verbose {
		fmt.Fprintln(os.Stderr, "=== CGI Environment ===")
		for _, e := range cgiEnv {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
		fmt.Fprintln(os.Stderr, "=======================")
	}

	// Capture stdout and stderr
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader(cfg.body)

	// Configure WASI module
	fsConfig := wazero.NewFSConfig().
		WithDirMount(scriptDir, "/").
		WithDirMount("/tmp", "/tmp")

	if cfg.phpIni != "" {
		iniDir := filepath.Dir(cfg.phpIni)
		fsConfig = fsConfig.WithDirMount(iniDir, "/etc/php")
	}

	modConfig := wazero.NewModuleConfig().
		WithName("php-cgi").
		WithStdin(stdin).
		WithStdout(&stdout).
		WithStderr(&stderr).
		WithFSConfig(fsConfig).
		WithArgs("php-cgi")

	for _, env := range cgiEnv {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			modConfig = modConfig.WithEnv(parts[0], parts[1])
		}
	}

	// Instantiate and run
	mod, err := rt.InstantiateModule(ctx, compiled, modConfig)
	exitCode = 0
	if err != nil {
		// Check if it's an exit error (normal for CGI)
		if exitErr, ok := err.(*sys.ExitError); ok {
			exitCode = int(exitErr.ExitCode())
			if exitCode != 0 {
				err = fmt.Errorf("php-cgi exited with code %d", exitCode)
			} else {
				err = nil
			}
		} else {
			return 1, fmt.Errorf("instantiating module: %w", err)
		}
	}
	if mod != nil {
		mod.Close(ctx)
	}

	// Print stderr from PHP
	if stderr.Len() > 0 {
		fmt.Fprintln(os.Stderr, "=== PHP stderr ===")
		os.Stderr.Write(stderr.Bytes())
	}

	// Parse and print CGI response
	response := stdout.String()
	headerEnd := strings.Index(response, "\r\n\r\n")
	if headerEnd < 0 {
		headerEnd = strings.Index(response, "\n\n")
	}

	if cfg.verbose && headerEnd >= 0 {
		fmt.Fprintln(os.Stderr, "=== Response Headers ===")
		fmt.Fprintln(os.Stderr, response[:headerEnd])
		fmt.Fprintln(os.Stderr, "========================")
	}

	// Write response body to stdout
	if headerEnd >= 0 {
		bodyStart := headerEnd + 4
		if response[headerEnd] == '\n' {
			bodyStart = headerEnd + 2
		}
		io.WriteString(os.Stdout, response[bodyStart:])
	} else {
		io.WriteString(os.Stdout, response)
	}

	return exitCode, err
}
