// Integration test for WordPress on WASI PHP + SQLite.
//
// Requires a pre-built wordpress-profile wasm binary. Set WASM_PATH to
// override the default, or run via `make test-wordpress`.
//
// The test bootstraps WordPress in a temp directory (no network calls after
// the initial setup.sh run), boots the wazero-wordpress server in-process,
// completes the WP install wizard, and asserts that the front page renders.
//
// Build the binary first:
//
//	make build PROFILE=wordpress VERSION=8.3
//	cd examples/wazero-wordpress && ./setup.sh
//	make test-wordpress
package wordpress_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// repoRoot returns the repository root (two levels up from tests/wordpress/).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine source file path")
	}
	// file = .../tests/wordpress/main_test.go
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func wasmPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WASM_PATH"); p != "" {
		return p
	}
	root := repoRoot(t)
	// Default: php-8.3.30-wordpress.wasm; adjust if version changes.
	candidates, _ := filepath.Glob(filepath.Join(root, "out", "php-*-wordpress.wasm"))
	if len(candidates) > 0 {
		return candidates[0]
	}
	t.Skip("No wordpress WASM binary found. Build with: make build PROFILE=wordpress VERSION=8.3")
	return ""
}

// bootstrapWP runs setup.sh in dir to create www/ and data/ hierarchies.
// It respects WP_TARBALL_PATH and SQLITE_PLUGIN_VERSION env overrides.
func bootstrapWP(t *testing.T, dir string) {
	t.Helper()
	root := repoRoot(t)
	setupSh := filepath.Join(root, "examples", "wazero-wordpress", "setup.sh")
	if _, err := os.Stat(setupSh); err != nil {
		t.Fatalf("setup.sh not found: %v", err)
	}
	cmd := exec.Command("bash", setupSh)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"WP_HOME=http://127.0.0.1:0",  // overridden later; just needs a placeholder
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup.sh failed:\n%s\nerror: %v", out, err)
	}
}

// wpServer mirrors the server logic from examples/wazero-wordpress/main.go,
// embedded here so the test doesn't depend on package main.
type wpServer struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	docroot  string
	dataDir  string
	phpIni   string
	timeout  time.Duration
	mux      *http.ServeMux
}

func newWPServer(wasmFile, docroot, dataDir, phpIni string, timeout time.Duration) (*wpServer, error) {
	wasmBytes, err := os.ReadFile(wasmFile)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(wazero.NewCompilationCache()))
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		rt.Close(ctx)
		return nil, err
	}
	for _, sub := range []string{"database", "uploads"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0755); err != nil {
			rt.Close(ctx)
			return nil, err
		}
	}
	s := &wpServer{
		rt: rt, compiled: compiled,
		docroot: docroot, dataDir: dataDir, phpIni: phpIni,
		timeout: timeout,
	}
	return s, nil
}

func (s *wpServer) close() {
	ctx := context.Background()
	s.compiled.Close(ctx)
	s.rt.Close(ctx)
}

// ServeHTTP implements the same routing as examples/wazero-wordpress/main.go.
func (s *wpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	p := r.URL.Path
	if p == "" {
		p = "/"
	}

	// Block dotfiles.
	for _, seg := range strings.Split(filepath.Clean(p), string(os.PathSeparator)) {
		if strings.HasPrefix(seg, ".") && seg != "." {
			http.NotFound(w, r)
			return
		}
	}

	rel := strings.TrimPrefix(filepath.Clean(p), string(os.PathSeparator))
	blocked := []string{"wp-config.php", "wp-content/database/", "wp-content/db.php"}
	for _, b := range blocked {
		if rel == b || strings.HasPrefix(rel, b) {
			http.NotFound(w, r)
			return
		}
	}

	if strings.HasSuffix(p, ".php") {
		s.runScript(w, r, p, r.URL.RequestURI(), id)
		return
	}
	if info, err := os.Stat(filepath.Join(s.docroot, rel)); err == nil && !info.IsDir() {
		http.ServeFile(w, r, filepath.Join(s.docroot, rel))
		return
	}
	s.runScript(w, r, "/index.php", r.URL.RequestURI(), id)
}

func (s *wpServer) runScript(w http.ResponseWriter, r *http.Request, scriptURL, requestURI, id string) {
	var body bytes.Buffer
	if r.Body != nil {
		io.Copy(&body, r.Body)
		r.Body.Close()
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	wasiScript := "/srv/app" + scriptURL
	env := []string{
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=" + r.Proto,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"REQUEST_URI=" + requestURI,
		"SCRIPT_FILENAME=" + wasiScript,
		"SCRIPT_NAME=" + scriptURL,
		"DOCUMENT_ROOT=/srv/app",
		"REDIRECT_STATUS=200",
		"CONTENT_LENGTH=" + fmt.Sprintf("%d", body.Len()),
		"HTTP_HOST=" + r.Host,
		"SERVER_NAME=" + r.Host,
		"HTTPS=",
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		env = append(env, "CONTENT_TYPE="+ct)
	} else {
		env = append(env, "CONTENT_TYPE=")
	}
	if ck := r.Header.Get("Cookie"); ck != "" {
		env = append(env, "HTTP_COOKIE="+ck)
	}

	fsConfig := wazero.NewFSConfig().
		WithDirMount(s.docroot, "/srv/app").
		WithDirMount(s.dataDir, "/data").
		WithDirMount("/tmp", "/tmp")
	if s.phpIni != "" {
		fsConfig = fsConfig.WithDirMount(filepath.Dir(s.phpIni), "/etc/php")
	}

	var stdout, stderr bytes.Buffer
	mc := wazero.NewModuleConfig().
		WithName("php-cgi-"+id).
		WithStdin(bytes.NewReader(body.Bytes())).
		WithStdout(&stdout).
		WithStderr(&stderr).
		WithFSConfig(fsConfig).
		WithArgs("php-cgi")
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			mc = mc.WithEnv(parts[0], parts[1])
		}
	}

	mod, err := s.rt.InstantiateModule(ctx, s.compiled, mc)
	if err != nil {
		if ctx.Err() != nil {
			http.Error(w, "timeout", http.StatusGatewayTimeout)
			return
		}
	}
	if mod != nil {
		mod.Close(ctx)
	}

	if stderr.Len() > 0 {
		fmt.Fprintf(os.Stderr, "[%s] PHP stderr: %s\n", id, stderr.String())
	}

	resp := stdout.String()
	sep := strings.Index(resp, "\r\n\r\n")
	offset := 4
	if sep < 0 {
		sep = strings.Index(resp, "\n\n")
		offset = 2
	}
	if sep < 0 {
		io.WriteString(w, resp)
		return
	}
	status := http.StatusOK
	for _, line := range strings.Split(resp[:sep], "\n") {
		line = strings.TrimRight(line, "\r")
		if ci := strings.Index(line, ":"); ci > 0 {
			k := strings.TrimSpace(line[:ci])
			v := strings.TrimSpace(line[ci+1:])
			switch strings.ToLower(k) {
			case "status":
				if code := strings.SplitN(v, " ", 2)[0]; len(code) == 3 {
					fmt.Sscanf(code, "%d", &status)
				}
			case "location":
				w.Header().Set("Location", v)
			case "set-cookie":
				w.Header().Add("Set-Cookie", v)
			case "content-type":
				w.Header().Set("Content-Type", v)
			default:
				w.Header().Set(k, v)
			}
		}
	}
	w.WriteHeader(status)
	io.WriteString(w, resp[sep+offset:])
}

// TestWordPressInstall boots WordPress, runs the install wizard, and checks
// that the front page renders with the expected title.
func TestWordPressInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WordPress integration test in short mode")
	}

	wasm := wasmPath(t)
	root := repoRoot(t)

	// Use a temp dir for the WordPress files so the test is isolated.
	tmpDir := t.TempDir()

	// Copy setup.sh's expected directory layout. We can't easily run setup.sh
	// from an arbitrary tmpDir because it contains relative paths to www/ and
	// data/. Instead, point the server at the pre-bootstrapped example dir.
	// The test therefore requires `cd examples/wazero-wordpress && ./setup.sh`
	// to have been run first (or WP_PREBUILT=1 env to be set).
	exampleDir := filepath.Join(root, "examples", "wazero-wordpress")
	www := filepath.Join(exampleDir, "www")
	data := filepath.Join(exampleDir, "data")
	phpIni := filepath.Join(exampleDir, "php.ini")

	if _, err := os.Stat(filepath.Join(www, "wp-load.php")); err != nil {
		if os.Getenv("WP_PREBUILT") != "1" {
			t.Log("www/ not found. Running setup.sh in a temp dir (needs network)...")
			// Run setup.sh from tmpDir so we don't pollute the example dir in CI.
			if err := os.Symlink(filepath.Join(exampleDir, "setup.sh"), filepath.Join(tmpDir, "setup.sh")); err != nil {
				// Fallback: just run from exampleDir
				bootstrapWP(t, exampleDir)
				www = filepath.Join(exampleDir, "www")
				data = filepath.Join(exampleDir, "data")
			} else {
				bootstrapWP(t, tmpDir)
				www = filepath.Join(tmpDir, "www")
				data = filepath.Join(tmpDir, "data")
			}
		} else {
			t.Skip("www/ not bootstrapped and WP_PREBUILT=1; run setup.sh first")
		}
	}

	srv, err := newWPServer(wasm, www, data, phpIni, 60*time.Second)
	if err != nil {
		t.Fatalf("creating WP server: %v", err)
	}
	defer srv.close()

	// Bind a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	baseURL := "http://" + ln.Addr().String()

	httpSrv := &http.Server{Handler: srv}
	go httpSrv.Serve(ln)
	defer httpSrv.Close()

	client := &http.Client{
		Timeout: 90 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // follow redirects
		},
	}

	// --- Step 1: install page should be reachable ---
	t.Run("install_page_loads", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/wp-admin/install.php")
		if err != nil {
			t.Fatalf("GET install.php: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d\n%s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "WordPress") {
			t.Fatalf("install page missing 'WordPress': %s", body[:min(500, len(body))])
		}
	})

	siteTitle := "WASI WordPress Test"
	adminUser := "testadmin"
	adminPass := "TestPass123!"
	adminEmail := "test@example.com"

	// --- Step 2: submit the install form ---
	t.Run("install_form_submit", func(t *testing.T) {
		form := url.Values{
			"weblog_title":    {siteTitle},
			"user_name":       {adminUser},
			"admin_password":  {adminPass},
			"admin_password2": {adminPass},
			"admin_email":     {adminEmail},
			"Submit":          {"Install WordPress"},
		}
		resp, err := client.PostForm(baseURL+"/wp-admin/install.php?step=2", form)
		if err != nil {
			t.Fatalf("POST install step 2: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d\n%s", resp.StatusCode, body[:min(500, len(body))])
		}
		if !strings.Contains(string(body), "Success") && !strings.Contains(string(body), "success") {
			t.Logf("Install response (first 500 bytes): %s", body[:min(500, len(body))])
			// Not a fatal failure — WP may have already been installed.
		}
	})

	// --- Step 3: front page should render ---
	t.Run("front_page_renders", func(t *testing.T) {
		// Give WP a moment to finish writing the SQLite db.
		time.Sleep(500 * time.Millisecond)
		resp, err := client.Get(baseURL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		if !strings.Contains(string(body), siteTitle) {
			t.Fatalf("front page title not found.\nExpected to contain: %q\nFirst 1000 bytes:\n%s",
				siteTitle, body[:min(1000, len(body))])
		}
		t.Logf("Front page OK: %d bytes", len(body))
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
