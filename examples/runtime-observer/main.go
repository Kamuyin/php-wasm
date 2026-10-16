// runtime-observer runs one CGI request with a private native observation host.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runtimehost "github.com/php-wasm/runtime/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	wasmPath := flag.String("wasm", "../../out/php-8.3.30-honeypot.wasm", "Instrumented PHP WASM binary")
	script := flag.String("script", "", "Application entrypoint; its directory is mounted read-only at /app")
	policy := flag.String("policy", "", "Host configuration JSON (not mounted inside PHP)")
	events := flag.String("events", "observations.jsonl", "Host-only observation output (created exclusively)")
	query := flag.String("query", "", "CGI query string")
	body := flag.String("body", "", "Form body; when nonempty the request uses POST")
	data := flag.String("data", "", "Optional instance data directory mounted read/write at /data")
	flag.Parse()
	if *script == "" {
		return fmt.Errorf("--script is required")
	}
	if len(*body) > 8<<20 {
		return fmt.Errorf("request body exceeds 8 MiB")
	}
	configuration := runtimehost.Config{Version: 1}
	if *policy != "" {
		file, err := os.Open(*policy)
		if err != nil {
			return err
		}
		configuration, err = runtimehost.DecodeConfig(file)
		file.Close()
		if err != nil {
			return err
		}
	}
	absolute, err := filepath.Abs(*script)
	if err != nil {
		return err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	appDir := filepath.Dir(absolute)
	mounts := []string{appDir}
	if *data != "" {
		if err := os.MkdirAll(*data, 0700); err != nil {
			return err
		}
		resolved, err := canonicalPath(*data)
		if err != nil {
			return err
		}
		if within(appDir, resolved) || within(resolved, appDir) {
			return fmt.Errorf("application and data directories must not overlap")
		}
		mounts = append(mounts, resolved)
		*data = resolved
	}
	// Resolve aliases before checking every guest-visible mount.
	for _, path := range []string{*policy, *events} {
		if path == "" {
			continue
		}
		abs, err := canonicalPath(path)
		if err != nil {
			return err
		}
		for _, mount := range mounts {
			if within(mount, abs) {
				return fmt.Errorf("policy and event files must be outside all guest mounts")
			}
		}
	}
	output, err := os.OpenFile(*events, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	request := make([]byte, 16)
	if _, err = rand.Read(request); err != nil {
		return err
	}
	requestID := hex.EncodeToString(request)
	encoder := json.NewEncoder(output)
	var analyzer runtimehost.Analyzer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(4096))
	defer runtime.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, runtime)
	host := runtimehost.Host{
		Config: func(context.Context, api.Module) (runtimehost.Config, error) { return configuration, nil },
		Emit: func(_ context.Context, _ api.Module, event json.RawMessage) error {
			findings, err := analyzer.Consume(event)
			if err != nil {
				return err
			}
			return encoder.Encode(struct {
				RequestID  string                `json:"request_id"`
				Event      json.RawMessage       `json:"event"`
				Findings   []runtimehost.Finding `json:"findings,omitempty"`
				ObservedAt time.Time             `json:"observed_at"`
			}{requestID, event, findings, time.Now().UTC()})
		},
		Error: func(_ context.Context, err error) { fmt.Fprintln(os.Stderr, "runtime host:", err) },
	}
	if err = host.Register(ctx, runtime); err != nil {
		return err
	}
	binary, err := os.ReadFile(*wasmPath)
	if err != nil {
		return err
	}
	compiled, err := runtime.CompileModule(ctx, binary)
	if err != nil {
		return err
	}
	defer compiled.Close(ctx)
	temporary, err := os.MkdirTemp("", "php-wasm-request-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	fs := wazero.NewFSConfig().WithReadOnlyDirMount(appDir, "/app").WithDirMount(temporary, "/tmp")
	if *data != "" {
		if err := os.MkdirAll(*data, 0700); err != nil {
			return err
		}
		fs = fs.WithDirMount(*data, "/data")
	}
	method := "GET"
	if *body != "" {
		method = "POST"
	}
	config := wazero.NewModuleConfig().WithName("php").WithFSConfig(fs).
		WithArgs("php-cgi", "-n", "-d", "memory_limit=128M", "-d", "display_errors=0").
		WithStdout(&boundedWriter{Writer: os.Stdout, remaining: 32 << 20}).WithStderr(os.Stderr).
		WithStdin(strings.NewReader(*body)).WithEnv("USE_ZEND_ALLOC", "0").
		WithEnv("REQUEST_METHOD", method).WithEnv("QUERY_STRING", *query).
		WithEnv("SCRIPT_FILENAME", "/app/"+filepath.Base(absolute)).WithEnv("DOCUMENT_ROOT", "/app").
		WithEnv("REQUEST_URI", "/"+filepath.Base(absolute)+"?"+*query).
		WithEnv("REDIRECT_STATUS", "200").WithEnv("SERVER_PROTOCOL", "HTTP/1.1").
		WithEnv("GATEWAY_INTERFACE", "CGI/1.1").WithEnv("SERVER_NAME", "localhost").
		WithEnv("CONTENT_TYPE", "application/x-www-form-urlencoded").WithEnv("CONTENT_LENGTH", strconv.Itoa(len(*body)))
	_, err = runtime.InstantiateModule(ctx, compiled, config)
	var exit *sys.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 0 {
		return nil
	}
	return err
}

type boundedWriter struct {
	io.Writer
	remaining int
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("response limit exceeded")
	}
	n, err := w.Writer.Write(p)
	w.remaining -= n
	return n, err
}
