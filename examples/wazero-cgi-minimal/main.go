// wazero-cgi-minimal: minimal HTTP server that serves PHP scripts via php-cgi WASM.
// Compiles the WASM module once at startup, instantiates a fresh module per request.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/php-wasm/examples/cgi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() {
	wasmPath := flag.String("wasm", "php.wasm", "Path to php-cgi .wasm binary")
	docroot := flag.String("docroot", "./www", "PHP document root")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	timeout := flag.Duration("timeout", 10*time.Second, "Per-request watchdog timeout")
	phpIni := flag.String("php-ini", "", "Path to php.ini (optional)")
	flag.Parse()

	srv, err := newServer(*wasmPath, *docroot, *phpIni, *timeout)
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer srv.close()

	log.Printf("php-wasm server listening on %s (docroot: %s)", *addr, *docroot)
	log.Printf("WASM binary: %s", *wasmPath)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// server holds the compiled WASM module and serves HTTP requests.
type server struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	docroot  string
	phpIni   string
	timeout  time.Duration
}

func newServer(wasmPath, docroot, phpIni string, timeout time.Duration) (*server, error) {
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("reading wasm binary: %w", err)
	}

	docroot, err = filepath.Abs(docroot)
	if err != nil {
		return nil, fmt.Errorf("resolving docroot: %w", err)
	}

	if _, err := os.Stat(docroot); err != nil {
		return nil, fmt.Errorf("docroot not accessible: %w", err)
	}

	ctx := context.Background()

	// Compilation cache persists the compiled module in memory across requests.
	cache := wazero.NewCompilationCache()

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(cache))

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	log.Printf("Compiling WASM module from %s ...", wasmPath)
	start := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compiling wasm: %w", err)
	}
	log.Printf("WASM compilation took %s", time.Since(start).Round(time.Millisecond))

	return &server{
		rt:       rt,
		compiled: compiled,
		docroot:  docroot,
		phpIni:   phpIni,
		timeout:  timeout,
	}, nil
}

func (s *server) close() {
	ctx := context.Background()
	s.compiled.Close(ctx)
	s.rt.Close(ctx)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := fmt.Sprintf("%d", time.Now().UnixNano())

	// Resolve script path
	urlPath := r.URL.Path
	if urlPath == "/" || urlPath == "" {
		urlPath = "/index.php"
	}
	if !strings.HasSuffix(urlPath, ".php") {
		http.NotFound(w, r)
		return
	}

	scriptPath := filepath.Join(s.docroot, filepath.Clean(urlPath))

	// Security: ensure script is under docroot
	if !strings.HasPrefix(scriptPath, s.docroot+string(os.PathSeparator)) &&
		scriptPath != s.docroot {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}

	// Read request body
	var bodyBuf bytes.Buffer
	if r.Body != nil {
		io.Copy(&bodyBuf, r.Body)
		r.Body.Close()
	}

	// Build CGI environment from HTTP request
	cgiEnv := buildCGIEnv(r, scriptPath, s.docroot, bodyBuf.Len(), requestID)

	// Per-request timeout watchdog
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// Capture PHP output
	var stdout, stderr bytes.Buffer
	stdin := bytes.NewReader(bodyBuf.Bytes())

	fsConfig := wazero.NewFSConfig().
		WithDirMount(s.docroot, "/srv/app").
		WithDirMount("/tmp", "/tmp")

	if s.phpIni != "" {
		fsConfig = fsConfig.WithDirMount(filepath.Dir(s.phpIni), "/etc/php")
	}

	modConfig := wazero.NewModuleConfig().
		WithName("php-cgi-" + requestID).
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

	// Instantiate a fresh module for this request
	mod, err := s.rt.InstantiateModule(ctx, s.compiled, modConfig)
	if err != nil {
		if exitErr, ok := err.(*sys.ExitError); ok {
			if exitErr.ExitCode() != 0 {
				log.Printf("[%s] php-cgi exit code %d", requestID, exitErr.ExitCode())
			}
		} else if ctx.Err() != nil {
			log.Printf("[%s] request timeout after %s", requestID, s.timeout)
			http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
			return
		} else {
			log.Printf("[%s] wasm error: %v", requestID, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}
	if mod != nil {
		mod.Close(ctx)
	}

	// Log PHP errors (stderr)
	if stderr.Len() > 0 {
		log.Printf("[%s] PHP stderr: %s", requestID, stderr.String())
	}

	// Parse CGI response headers + body
	cgi.WriteCGIResponse(w, stdout.String(), requestID)
}

// buildCGIEnv builds CGI/1.1 environment variables from an HTTP request.
func buildCGIEnv(r *http.Request, scriptPath, docroot string, contentLength int, requestID string) []string {
	env := []string{
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=" + r.Proto,
		"SERVER_SOFTWARE=php-wasm/1.0",
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"REQUEST_URI=" + r.URL.RequestURI(),
		"SCRIPT_FILENAME=" + scriptPath,
		"SCRIPT_NAME=" + r.URL.Path,
		"DOCUMENT_ROOT=" + docroot,
		"REDIRECT_STATUS=200",
		"CONTENT_LENGTH=" + strconv.Itoa(contentLength),
		"HTTP_HOST=" + r.Host,
		"SERVER_NAME=" + r.Host,
		// Request ID for tracing through WASM boundary
		"HTTP_X_REQUEST_ID=" + requestID,
	}

	// Content-Type
	if ct := r.Header.Get("Content-Type"); ct != "" {
		env = append(env, "CONTENT_TYPE="+ct)
	} else {
		env = append(env, "CONTENT_TYPE=")
	}

	// Remote address
	remoteAddr := r.RemoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx >= 0 {
		env = append(env, "REMOTE_ADDR="+remoteAddr[:idx])
		env = append(env, "REMOTE_PORT="+remoteAddr[idx+1:])
	} else {
		env = append(env, "REMOTE_ADDR="+remoteAddr)
	}

	// Propagate selected HTTP headers as HTTP_* vars
	for _, header := range []string{
		"Accept", "Accept-Charset", "Accept-Encoding", "Accept-Language",
		"Authorization", "Cache-Control", "Cookie", "Referer", "User-Agent",
	} {
		if val := r.Header.Get(header); val != "" {
			key := "HTTP_" + strings.ToUpper(strings.ReplaceAll(header, "-", "_"))
			env = append(env, key+"="+val)
		}
	}

	return env
}

