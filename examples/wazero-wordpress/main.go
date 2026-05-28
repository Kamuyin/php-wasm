// wazero-wordpress: HTTP server that serves a WordPress site via php-cgi WASM.
//
// WordPress runs against a SQLite database using the sqlite-database-integration
// plugin (wp-content/db.php). Run setup.sh first to bootstrap the WP files.
//
// Usage:
//
//	go run main.go --wasm ../../out/php-8.3.30-wordpress.wasm [--addr :8080]
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
	wasmPath := flag.String("wasm", "../../out/php-8.3.30-wordpress.wasm", "Path to php-cgi .wasm binary")
	docroot := flag.String("docroot", "./www", "WordPress document root (populated by setup.sh)")
	dataDir := flag.String("data-dir", "./data", "Persistent data dir (sqlite DB + uploads)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	timeout := flag.Duration("timeout", 300*time.Second, "Per-request timeout (WP install needs ~30-60s)")
	phpIni := flag.String("php-ini", "./php.ini", "Path to php.ini")
	flag.Parse()

	srv, err := newServer(*wasmPath, *docroot, *dataDir, *phpIni, *timeout)
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer srv.close()

	log.Printf("WordPress WASM server on %s", *addr)
	log.Printf("  WASM    : %s", *wasmPath)
	log.Printf("  docroot : %s", *docroot)
	log.Printf("  data    : %s", *dataDir)
	log.Printf("  Open http://localhost%s/wp-admin/install.php to install WordPress", *addr)

	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

type server struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	docroot  string
	dataDir  string
	phpIni   string
	timeout  time.Duration
}

func newServer(wasmPath, docroot, dataDir, phpIni string, timeout time.Duration) (*server, error) {
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("reading wasm binary: %w", err)
	}

	docroot, err = filepath.Abs(docroot)
	if err != nil {
		return nil, fmt.Errorf("resolving docroot: %w", err)
	}
	if _, err := os.Stat(docroot); err != nil {
		return nil, fmt.Errorf("docroot not accessible (run setup.sh first): %w", err)
	}

	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolving data dir: %w", err)
	}
	for _, sub := range []string{"database", "uploads"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0755); err != nil {
			return nil, fmt.Errorf("creating data/%s: %w", sub, err)
		}
	}

	ctx := context.Background()
	cache := wazero.NewCompilationCache()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	log.Printf("Compiling WASM module (this takes a few seconds on first run)...")
	start := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compiling wasm: %w", err)
	}
	log.Printf("Ready in %s", time.Since(start).Round(time.Millisecond))

	return &server{
		rt:       rt,
		compiled: compiled,
		docroot:  docroot,
		dataDir:  dataDir,
		phpIni:   phpIni,
		timeout:  timeout,
	}, nil
}

func (s *server) close() {
	ctx := context.Background()
	s.compiled.Close(ctx)
	s.rt.Close(ctx)
}

var blockedPaths = []string{
	"wp-config.php",
	"wp-content/database/",
	"wp-content/db.php",
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := fmt.Sprintf("%d", time.Now().UnixNano())

	urlPath := r.URL.Path
	if urlPath == "" {
		urlPath = "/"
	}

	if cgi.HasDotfileSegment(urlPath) {
		http.NotFound(w, r)
		return
	}
	if cgi.BlockedByPath(urlPath, blockedPaths) {
		http.NotFound(w, r)
		return
	}

	if strings.HasSuffix(urlPath, ".php") {
		s.servePHP(w, r, urlPath, requestID)
		return
	}

	hostPath := filepath.Join(s.docroot, filepath.Clean(urlPath))
	if info, err := os.Stat(hostPath); err == nil && !info.IsDir() {
		http.ServeFile(w, r, hostPath)
		return
	}

	// REQUEST_URI kept as-is so WP's router sees the original permalink.
	s.servePHPWithRewrite(w, r, "/index.php", r.URL.RequestURI(), requestID)
}

func (s *server) servePHP(w http.ResponseWriter, r *http.Request, urlPath, requestID string) {
	scriptPath := filepath.Join(s.docroot, filepath.Clean(urlPath))

	if !strings.HasPrefix(scriptPath, s.docroot+string(os.PathSeparator)) &&
		scriptPath != s.docroot {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		// Missing .php → also try the WP rewrite (e.g. /wp-json/... calls)
		s.servePHPWithRewrite(w, r, "/index.php", r.URL.RequestURI(), requestID)
		return
	}

	s.runPHP(w, r, scriptPath, r.URL.Path, r.URL.RequestURI(), requestID)
}

func (s *server) servePHPWithRewrite(w http.ResponseWriter, r *http.Request, scriptURL, requestURI, requestID string) {
	scriptPath := filepath.Join(s.docroot, filepath.Clean(scriptURL))
	s.runPHP(w, r, scriptPath, scriptURL, requestURI, requestID)
}

func (s *server) runPHP(w http.ResponseWriter, r *http.Request, scriptPath, scriptName, requestURI, requestID string) {
	var bodyBuf bytes.Buffer
	if r.Body != nil {
		io.Copy(&bodyBuf, r.Body)
		r.Body.Close()
	}

	cgiEnv := buildCGIEnv(r, scriptPath, scriptName, requestURI, s.docroot, bodyBuf.Len(), requestID)

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	stdin := bytes.NewReader(bodyBuf.Bytes())

	// Nested mounts under an existing pre-open are unreliable in wazero; flat pre-opens are safe.
	// /dev is needed for random_bytes(): csprng.c falls back to /dev/urandom on WASI.
	fsConfig := wazero.NewFSConfig().
		WithDirMount(s.docroot, "/srv/app").
		WithDirMount(s.dataDir, "/data").
		WithDirMount("/tmp", "/tmp").
		WithDirMount("/dev", "/dev")

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

	mod, err := s.rt.InstantiateModule(ctx, s.compiled, modConfig)
	if mod != nil {
		mod.Close(ctx)
	}

	if stderr.Len() > 0 {
		log.Printf("[%s] PHP stderr:\n%s", requestID, stderr.String())
	}

	if err != nil {
		if _, ok := err.(*sys.ExitError); !ok {
			if ctx.Err() != nil {
				log.Printf("[%s] timeout after %s", requestID, s.timeout)
				http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
				return
			}
			log.Printf("[%s] wasm error: %v", requestID, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	cgi.WriteCGIResponse(w, stdout.String(), requestID)
}

func buildCGIEnv(r *http.Request, scriptPath, scriptName, requestURI, docroot string, contentLength int, requestID string) []string {
	// SCRIPT_FILENAME must be the WASI path (under /srv/app), not the host path.
	wasiScript := "/srv/app" + scriptName

	env := []string{
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=" + r.Proto,
		"SERVER_SOFTWARE=php-wasm-wordpress/1.0",
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"REQUEST_URI=" + requestURI,
		"SCRIPT_FILENAME=" + wasiScript,
		"SCRIPT_NAME=" + scriptName,
		"DOCUMENT_ROOT=/srv/app",
		"REDIRECT_STATUS=200",
		"CONTENT_LENGTH=" + strconv.Itoa(contentLength),
		"HTTP_HOST=" + r.Host,
		"SERVER_NAME=" + r.Host,
		// Empty string (not "off") — WP checks isset($_SERVER['HTTPS']) && $_SERVER['HTTPS'] != 'off'
		"HTTPS=",
		"HTTP_X_REQUEST_ID=" + requestID,
	}

	if ct := r.Header.Get("Content-Type"); ct != "" {
		env = append(env, "CONTENT_TYPE="+ct)
	} else {
		env = append(env, "CONTENT_TYPE=")
	}

	remoteAddr := r.RemoteAddr
	if idx := strings.LastIndex(remoteAddr, ":"); idx >= 0 {
		env = append(env, "REMOTE_ADDR="+remoteAddr[:idx])
	} else {
		env = append(env, "REMOTE_ADDR="+remoteAddr)
	}

	for _, header := range []string{
		"Accept", "Accept-Language", "Accept-Encoding",
		"Authorization", "Cache-Control", "Cookie",
		"Referer", "User-Agent", "X-Forwarded-For",
	} {
		if val := r.Header.Get(header); val != "" {
			key := "HTTP_" + strings.ToUpper(strings.ReplaceAll(header, "-", "_"))
			env = append(env, key+"="+val)
		}
	}

	return env
}

