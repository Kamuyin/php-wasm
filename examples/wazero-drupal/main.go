// wazero-drupal: HTTP server that serves a Drupal site via php-cgi WASM.
//
// PHP is kept alive as a persistent FastCGI process (one WASM instance per
// server lifetime). Each HTTP request sends a FastCGI request over a pipe pair
// and reads the response back, avoiding the full PHP boot cost per page.
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/php-wasm/examples/cgi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() {
	// Directories created via WASI path_create_directory get mode 0777 before
	// umask. PHP's is_writable() on WASI checks mode bits directly, so 0700
	// directories look unwritable to the installer without this.
	syscall.Umask(0022)
	wasmPath := flag.String("wasm", "../../out/php-8.3.30-drupal.wasm", "Path to php-cgi .wasm binary")
	docroot := flag.String("docroot", "./www", "Drupal document root (populated by setup.sh)")
	dataDir := flag.String("data-dir", "./data", "Persistent data dir (sqlite DB + private files)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	timeout := flag.Duration("timeout", 300*time.Second, "Per-request timeout")
	phpIni := flag.String("php-ini", "./php.ini", "Path to php.ini")
	flag.Parse()

	srv, err := newServer(*wasmPath, *docroot, *dataDir, *phpIni, *timeout)
	if err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	defer srv.close()

	log.Printf("Drupal WASM server on %s", *addr)
	log.Printf("  WASM    : %s", *wasmPath)
	log.Printf("  docroot : %s", *docroot)
	log.Printf("  data    : %s", *dataDir)
	log.Printf("  Open http://localhost%s/core/install.php to install Drupal", *addr)

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
	// phpMu serializes requests: one FastCGI exchange at a time, and guards
	// the PHP process state (fcgiIn/fcgiOut/phpDone) during restart.
	phpMu     sync.Mutex
	fcgiIn    io.WriteCloser // Go writes FastCGI request records here (→ PHP stdin)
	fcgiOut   io.ReadCloser  // Go reads FastCGI response records here (← PHP stdout)
	phpDone   chan struct{}   // closed when the PHP WASM goroutine exits
	phpCancel context.CancelFunc
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
	for _, sub := range []string{"database", "private"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0755); err != nil {
			return nil, fmt.Errorf("creating data/%s: %w", sub, err)
		}
	}

	if err := os.MkdirAll("/tmp/opcache", 0755); err != nil {
		return nil, fmt.Errorf("creating opcache dir: %w", err)
	}

	ctx := context.Background()
	cacheDir := filepath.Join(os.TempDir(), "php-wasm-drupal-cache")
	cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		log.Printf("Warning: could not create compilation cache dir %s: %v; falling back to in-memory cache", cacheDir, err)
		cache = wazero.NewCompilationCache()
	}
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	log.Printf("Compiling WASM module (first run takes a few seconds, then cached)...")
	start := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compiling wasm: %w", err)
	}
	log.Printf("Ready in %s", time.Since(start).Round(time.Millisecond))

	srv := &server{
		rt:       rt,
		compiled: compiled,
		docroot:  docroot,
		dataDir:  dataDir,
		phpIni:   phpIni,
		timeout:  timeout,
	}
	if err := srv.startPHP(); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	return srv, nil
}

// startPHP launches the persistent PHP FastCGI process.
// Must be called with phpMu held (or before the server is shared).
func (s *server) startPHP() error {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return fmt.Errorf("create stdout pipe: %w", err)
	}

	s.fcgiIn = stdinW
	s.fcgiOut = stdoutR
	phpDone := make(chan struct{})
	s.phpDone = phpDone

	ctx, cancel := context.WithCancel(context.Background())
	s.phpCancel = cancel

	fsConfig := wazero.NewFSConfig().
		WithDirMount(s.docroot, "/srv/app").
		WithDirMount(s.dataDir, "/data").
		WithDirMount("/tmp", "/tmp")
	if s.phpIni != "" {
		fsConfig = fsConfig.WithDirMount(filepath.Dir(s.phpIni), "/etc/php")
	}

	modConfig := wazero.NewModuleConfig().
		WithName(fmt.Sprintf("php-fcgi-%d", time.Now().UnixNano())).
		WithStdin(stdinR).
		WithStdout(stdoutW).
		WithStderr(os.Stderr).
		WithFSConfig(fsConfig).
		WithRandSource(crand.Reader).
		WithArgs("php-cgi").
		WithEnv("PHP_FCGI_FORCE", "1").     // activates our WASI FastCGI patch
		WithEnv("PHP_FCGI_MAX_REQUESTS", "0"). // unlimited; we manage the lifecycle
		WithEnv("PHP_WASM_WASI", "1")

	go func() {
		defer close(phpDone)
		defer stdoutW.Close()
		defer stdinR.Close()

		mod, err := s.rt.InstantiateModule(ctx, s.compiled, modConfig)
		if mod != nil {
			mod.Close(ctx)
		}
		if err != nil {
			if exitErr, ok := err.(*sys.ExitError); ok {
				log.Printf("PHP process exited with code %d", exitErr.ExitCode())
			} else {
				log.Printf("PHP process terminated with error: %v", err)
			}
		} else {
			log.Printf("PHP process exited cleanly")
		}
	}()

	return nil
}

// ensurePHP restarts the PHP process if it has exited.
// Must be called with phpMu held.
func (s *server) ensurePHP() error {
	select {
	case <-s.phpDone:
		log.Printf("PHP process was down, restarting...")
		return s.startPHP()
	default:
		return nil
	}
}

// killAndRestartPHP closes the current PHP pipes (causing PHP to exit) and
// starts a fresh process. Must be called with phpMu held.
func (s *server) killAndRestartPHP() {
	if s.fcgiOut != nil {
		s.fcgiOut.Close()
	}
	if s.fcgiIn != nil {
		s.fcgiIn.Close()
	}
	select {
	case <-s.phpDone:
	case <-time.After(5 * time.Second):
		log.Printf("PHP did not exit within 5s after pipe close; forcing cancel")
		if s.phpCancel != nil {
			s.phpCancel()
		}
		select {
		case <-s.phpDone:
		case <-time.After(5 * time.Second):
			log.Printf("PHP still did not exit after cancel; starting a replacement process")
		}
	}
	if err := s.startPHP(); err != nil {
		log.Printf("Failed to restart PHP: %v", err)
	}
}

func (s *server) close() {
	s.phpMu.Lock()
	defer s.phpMu.Unlock()

	if s.fcgiIn != nil {
		s.fcgiIn.Close()
	}
	if s.phpDone != nil {
		select {
		case <-s.phpDone:
		case <-time.After(10 * time.Second):
			log.Printf("PHP did not exit cleanly; forcing")
			if s.phpCancel != nil {
				s.phpCancel()
			}
		}
	}

	ctx := context.Background()
	s.compiled.Close(ctx)
	s.rt.Close(ctx)
}

var blockedPaths = []string{
	"sites/default/settings.php",
	"sites/default/services.yml",
	"sites/default/files/private/",
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

	s.servePHPWithRewrite(w, r, "/index.php", r.URL.RequestURI(), requestID)
}

func (s *server) servePHP(w http.ResponseWriter, r *http.Request, urlPath, requestID string) {
	scriptPath := filepath.Join(s.docroot, filepath.Clean(urlPath))

	if !strings.HasPrefix(scriptPath, s.docroot+string(os.PathSeparator)) && scriptPath != s.docroot {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		s.servePHPWithRewrite(w, r, "/index.php", r.URL.RequestURI(), requestID)
		return
	}

	s.runPHP(w, r, scriptPath, urlPath, r.URL.RequestURI(), requestID)
}

func (s *server) servePHPWithRewrite(w http.ResponseWriter, r *http.Request, scriptURL, requestURI, requestID string) {
	scriptPath := filepath.Join(s.docroot, filepath.Clean(scriptURL))
	s.runPHP(w, r, scriptPath, scriptURL, requestURI, requestID)
}

func (s *server) runPHP(w http.ResponseWriter, r *http.Request, scriptPath, scriptName, requestURI, requestID string) {
	s.phpMu.Lock()
	defer s.phpMu.Unlock()

	var bodyBuf bytes.Buffer
	if r.Body != nil {
		io.Copy(&bodyBuf, r.Body)
		r.Body.Close()
	}

	if err := s.ensurePHP(); err != nil {
		log.Printf("[%s] PHP restart failed: %v", requestID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	env := buildCGIEnv(r, scriptName, requestURI, bodyBuf.Len(), requestID)

	type result struct {
		kind string
		resp *fcgiResponse
		err  error
	}
	done := make(chan result, 2)
	fcgiIn := s.fcgiIn
	fcgiOut := s.fcgiOut

	go func() {
		err := writeFCGIRequest(fcgiIn, env, bodyBuf.Bytes())
		if err != nil {
			err = fmt.Errorf("fcgi write: %w", err)
		}
		done <- result{kind: "write", err: err}
	}()
	go func() {
		resp, err := readFCGIResponse(fcgiOut)
		done <- result{kind: "read", resp: resp, err: err}
	}()

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	var resp *fcgiResponse
	pending := 2
	for pending > 0 {
		select {
		case res := <-done:
			pending--
			if res.err != nil {
				log.Printf("[%s] fcgi %s error: %v — restarting PHP", requestID, res.kind, res.err)
				s.killAndRestartPHP()
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			if res.kind == "read" {
				resp = res.resp
			}
		case <-timer.C:
			log.Printf("[%s] PHP request timed out after %v — restarting PHP", requestID, s.timeout)
			s.killAndRestartPHP()
			http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
			return
		}
	}

	if resp == nil {
		log.Printf("[%s] fcgi read completed without a response — restarting PHP", requestID)
		s.killAndRestartPHP()
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if len(resp.stderr) > 0 {
		log.Printf("[%s] PHP stderr:\n%s", requestID, string(resp.stderr))
	}
	cgi.WriteCGIResponse(w, string(resp.stdout), requestID)
}

func buildCGIEnv(r *http.Request, scriptName, requestURI string, contentLength int, requestID string) []string {
	wasiScript := "/srv/app" + scriptName
	pathInfo := ""
	if strings.HasPrefix(r.URL.Path, scriptName) && len(r.URL.Path) > len(scriptName) {
		pathInfo = r.URL.Path[len(scriptName):]
	}
	serverName := r.Host
	serverPort := "80"
	if host, port, err := net.SplitHostPort(r.Host); err == nil {
		serverName = host
		serverPort = port
	}

	env := []string{
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=" + r.Proto,
		"SERVER_SOFTWARE=php-wasm-drupal/1.0",
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"REQUEST_URI=" + requestURI,
		"SCRIPT_FILENAME=" + wasiScript,
		"SCRIPT_NAME=" + scriptName,
		"PATH_INFO=" + pathInfo,
		"DOCUMENT_ROOT=/srv/app",
		"REDIRECT_STATUS=200",
		"CONTENT_LENGTH=" + strconv.Itoa(contentLength),
		"HTTP_HOST=" + r.Host,
		"SERVER_NAME=" + serverName,
		"SERVER_PORT=" + serverPort,
		"REQUEST_SCHEME=http",
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
		env = append(env, "REMOTE_PORT="+remoteAddr[idx+1:])
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
