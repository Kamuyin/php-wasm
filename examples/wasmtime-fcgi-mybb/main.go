package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/bytecodealliance/wasmtime-go/v20"
)

var (
	wasmPath   = flag.String("wasm", "../../build/php-8.3.30-wordpress.wasm", "Path to the PHP WASM binary")
	docRoot    = flag.String("docroot", "./www", "Path to the document root")
	dataDir    = flag.String("data-dir", "./data", "Path to the persistent data directory")
	addr       = flag.String("addr", ":8080", "HTTP server address")
	fcgiMu     sync.Mutex // Ensures serial execution since we have 1 persistent WASM instance
	fcgiIn     *os.File
	fcgiOut    *os.File
)

func main() {
	flag.Parse()

	log.Printf("Loading WASM module from %s...", *wasmPath)
	wasmBytes, err := os.ReadFile(*wasmPath)
	if err != nil {
		log.Fatalf("Failed to read wasm file: %v", err)
	}

	engine := wasmtime.NewEngine()
	module, err := wasmtime.NewModule(engine, wasmBytes)
	if err != nil {
		log.Fatalf("Failed to compile wasm module: %v", err)
	}

	// Create pipes for FastCGI communication
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		log.Fatalf("Failed to create stdin pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		log.Fatalf("Failed to create stdout pipe: %v", err)
	}

	// Setup WASI
	store := wasmtime.NewStore(engine)
	wasi := wasmtime.NewWasiConfig()
	wasi.SetArgv([]string{"php-cgi"})

	// Connect pipes using Linux procfs (works on WSL/Linux)
	if err := wasi.SetStdinFile(fmt.Sprintf("/proc/self/fd/%d", stdinR.Fd())); err != nil {
		log.Fatalf("Failed to set stdin: %v", err)
	}
	if err := wasi.SetStdoutFile(fmt.Sprintf("/proc/self/fd/%d", stdoutW.Fd())); err != nil {
		log.Fatalf("Failed to set stdout: %v", err)
	}
	wasi.InheritStderr()

	wasi.SetEnv(
		[]string{"PHP_FCGI_FORCE", "PHP_FCGI_MAX_REQUESTS", "PHP_WASM_WASI"},
		[]string{"1", "0", "1"},
	)

	// Preopen /etc/php (for php.ini)
	wd, _ := os.Getwd()
	if err := wasi.PreopenDir(wd, "/etc/php"); err != nil {
		log.Fatalf("Failed to preopen /etc/php: %v", err)
	}
	// Preopen /var/www/html (for document root)
	docRootAbs, _ := filepath.Abs(*docRoot)
	if err := wasi.PreopenDir(docRootAbs, "/var/www/html"); err != nil {
		log.Fatalf("Failed to preopen /var/www/html: %v", err)
	}
	// Preopen /data (for sqlite and uploads)
	dataDirAbs, _ := filepath.Abs(*dataDir)
	if err := wasi.PreopenDir(dataDirAbs, "/data"); err != nil {
		log.Fatalf("Failed to preopen /data: %v", err)
	}
	// Preopen /tmp
	tmpDir, _ := os.MkdirTemp("", "php-wasm-tmp-")
	if err := wasi.PreopenDir(tmpDir, "/tmp"); err != nil {
		log.Fatalf("Failed to preopen /tmp: %v", err)
	}
	// Preopen /dev for random_bytes()
	if err := wasi.PreopenDir("/dev", "/dev"); err != nil {
		log.Fatalf("Failed to preopen /dev: %v", err)
	}

	store.SetWasi(wasi)
	linker := wasmtime.NewLinker(engine)
	linker.DefineWasi()

	log.Println("Instantiating WASM module...")
	instance, err := linker.Instantiate(store, module)
	if err != nil {
		log.Fatalf("Failed to instantiate module: %v", err)
	}

	start := instance.GetFunc(store, "_start")
	if start == nil {
		log.Fatalf("Failed to find _start function")
	}

	// Run persistent WASM process in background
	go func() {
		defer stdinR.Close()
		defer stdoutW.Close()
		log.Println("Persistent FastCGI WASM instance started")
		_, err := start.Call(store)
		log.Fatalf("Persistent WASM instance exited: %v", err)
	}()

	fcgiIn = stdinW
	fcgiOut = stdoutR

	http.HandleFunc("/", handleRequest)

	log.Printf("Listening on http://127.0.0.1%s", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// Serve static files directly if they exist
	urlPath := r.URL.Path
	if urlPath == "/" || urlPath == "" {
		urlPath = "/index.php"
	}
	
	hostPath := filepath.Join(*docRoot, filepath.FromSlash(urlPath))
	if !strings.HasSuffix(urlPath, ".php") {
		if info, err := os.Stat(hostPath); err == nil && !info.IsDir() {
			http.ServeFile(w, r, hostPath)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusInternalServerError)
		return
	}
	r.Body.Close()

	scriptName := urlPath
	if !strings.HasSuffix(scriptName, ".php") {
		scriptName = "/index.php"
	}

	env := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_PROTOCOL":   r.Proto,
		"SERVER_SOFTWARE":   "wasmtime-fcgi-mybb",
		"REQUEST_METHOD":    r.Method,
		"QUERY_STRING":      r.URL.RawQuery,
		"REQUEST_URI":       r.URL.RequestURI(),
		"SCRIPT_FILENAME":   "/var/www/html" + scriptName,
		"SCRIPT_NAME":       scriptName,
		"DOCUMENT_ROOT":     "/var/www/html",
		"REDIRECT_STATUS":   "200",
		"CONTENT_LENGTH":    strconv.Itoa(len(body)),
		"CONTENT_TYPE":      r.Header.Get("Content-Type"),
		"HTTP_HOST":         r.Host,
		"SERVER_NAME":       r.Host,
		"REMOTE_ADDR":       "127.0.0.1",
	}

	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		env["REMOTE_ADDR"] = host
		env["REMOTE_PORT"] = port
	}

	for key, values := range r.Header {
		if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		env["HTTP_"+strings.ToUpper(strings.ReplaceAll(key, "-", "_"))] = strings.Join(values, ", ")
	}

	// Single request at a time due to single persistent WASM process
	fcgiMu.Lock()
	defer fcgiMu.Unlock()

	if err := writeFCGIRequest(fcgiIn, env, body); err != nil {
		log.Printf("FCGI write error: %v", err)
		http.Error(w, "FastCGI Gateway Error", http.StatusBadGateway)
		return
	}

	resp, err := readFCGIResponse(fcgiOut)
	if err != nil {
		log.Printf("FCGI read error: %v", err)
		http.Error(w, "FastCGI Response Error", http.StatusBadGateway)
		return
	}

	if len(resp.stderr) > 0 {
		log.Printf("PHP STDERR: %s", string(resp.stderr))
	}

	writeCGIResponse(w, resp.stdout)
}

func writeCGIResponse(w http.ResponseWriter, data []byte) {
	header, body := splitCGIResponse(data)
	statusCode := http.StatusOK
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "Status") {
			fields := strings.Fields(value)
			if len(fields) > 0 {
				if code, err := strconv.Atoi(fields[0]); err == nil {
					statusCode = code
				}
			}
			continue
		}
		w.Header().Add(name, value)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", mime.TypeByExtension(".html"))
	}
	if statusCode != http.StatusOK {
		w.WriteHeader(statusCode)
	}
	w.Write(body)
}

func splitCGIResponse(data []byte) (string, []byte) {
	if idx := bytes.Index(data, []byte("\r\n\r\n")); idx >= 0 {
		return string(data[:idx]), data[idx+4:]
	}
	if idx := bytes.Index(data, []byte("\n\n")); idx >= 0 {
		return string(data[:idx]), data[idx+2:]
	}
	return "Content-Type: text/html", data
}
