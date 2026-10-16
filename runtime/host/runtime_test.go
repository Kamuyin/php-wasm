package runtimehost

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

type observed struct {
	Request int
	Data    map[string]any
}

func runPHP(t *testing.T, source string, configs []Config, fastCGI bool) (string, []observed) {
	return runPHPDatabase(t, source, configs, fastCGI, nil)
}
func runPHPDatabase(t *testing.T, source string, configs []Config, fastCGI bool, database func(context.Context, api.Module, json.RawMessage) ([]byte, error)) (string, []observed) {
	t.Helper()
	binaryPath := os.Getenv("PHP_WASM_BINARY")
	if binaryPath == "" {
		t.Skip("set PHP_WASM_BINARY to the instrumented PHP 8.3 binary")
	}
	binaryData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "index.php"), []byte("<?php\n"+source), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "included.php"), []byte("<?php return 'included';"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(4096))
	defer runtime.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, runtime)
	var events []observed
	request := 0
	var hostErrors []error
	host := Host{
		Database: database,
		Config: func(context.Context, api.Module) (Config, error) {
			if request >= len(configs) {
				return Config{}, fmt.Errorf("unexpected request")
			}
			config := configs[request]
			if database != nil {
				if _, err := database(context.Background(), nil, json.RawMessage(`{"op":"reset"}`)); err != nil {
					return Config{}, err
				}
			}
			request++
			return config, nil
		},
		Emit: func(_ context.Context, _ api.Module, raw json.RawMessage) error {
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			events = append(events, observed{request, event})
			return nil
		},
		Error: func(_ context.Context, err error) { hostErrors = append(hostErrors, err) },
	}
	if err := host.Register(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	compiled, err := runtime.CompileModule(ctx, binaryData)
	if err != nil {
		t.Fatal(err)
	}
	defer compiled.Close(ctx)
	var stdout, stderr bytes.Buffer
	env := map[string]string{
		"SCRIPT_FILENAME": "/app/index.php", "REQUEST_METHOD": "GET", "REQUEST_URI": "/index.php",
		"QUERY_STRING": "code=" + url.QueryEscape("return 40 + 2;") + "&sqli=" + url.QueryEscape("' OR 1=1 -- ") + "&include=" + url.QueryEscape("/app/included.php"), "REDIRECT_STATUS": "200",
		"DOCUMENT_ROOT": "/app", "SERVER_NAME": "localhost", "SERVER_PROTOCOL": "HTTP/1.1",
		"GATEWAY_INTERFACE": "CGI/1.1", "USE_ZEND_ALLOC": "0",
	}
	var stdin bytes.Buffer
	if fastCGI {
		for i := range configs {
			stdin.Write(fcgiRequest(uint16(i+1), env))
		}
		env["PHP_FCGI_FORCE"] = "1"
		env["PHP_WASM_WASI"] = "1"
		env["PHP_FCGI_MAX_REQUESTS"] = fmt.Sprint(len(configs))
	}
	modConfig := wazero.NewModuleConfig().WithName("php").WithStdin(&stdin).WithStdout(&stdout).WithStderr(&stderr).
		WithFSConfig(wazero.NewFSConfig().WithReadOnlyDirMount(app, "/app").WithDirMount(t.TempDir(), "/tmp")).
		WithArgs("php-cgi", "-n", "-d", "memory_limit=128M", "-d", "display_errors=0")
	for key, value := range env {
		modConfig = modConfig.WithEnv(key, value)
	}
	_, err = runtime.InstantiateModule(ctx, compiled, modConfig)
	var exit *sys.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 0) {
		t.Fatalf("PHP: %v\nrequests: %d; host errors: %v\nevents: %v\nstderr: %s\nstdout: %s", err, request, hostErrors, events, stderr.String(), stdout.String())
	}
	if len(hostErrors) > 0 {
		t.Fatalf("host errors: %v", hostErrors)
	}
	if request != len(configs) {
		t.Fatalf("handled %d requests, want %d; stderr: %s", request, len(configs), stderr.String())
	}
	return stdout.String(), events
}

func TestNativeObservationAndInjection(t *testing.T) {
	configuration := Config{Version: 1, Bootstrap: `$GLOBALS['boot'] = 'host';`, Hooks: map[string]Hook{
		"target":           {Before: `$arguments[0]->value = 'live'; return ['arguments'=>[1=>'rewritten']];`, After: `return ['result'=>$result.':after', 'emit'=>['name'=>'custom', 'value'=>$object]];`},
		"sample::receiver": {Before: `$object->value = 'receiver';`, After: `return ['result'=>$result.':method'];`},
	}}
	out, events := runPHP(t, `
class Sample implements JsonSerializable {
    public $value = 'initial';
    public function jsonSerialize(): mixed { throw new Exception('observer invoked application serializer'); }
    public function __toString(): string { throw new Exception('observer invoked application string conversion'); }
    public function receiver(): string { return $this->value; }
}
function target($object, $text) { return $object->value.':'.$text; }
$jsonBefore = json_last_error();
$object = new Sample();
$result = target($object, 'original');
$receiver = $object->receiver();
$stored = file_put_contents('/tmp/sample', 'payload');
$read = file_get_contents('/tmp/sample');
$missing = @file_get_contents('/tmp/absent');
$db = new PDO('sqlite::memory:');
$sql = $db->query('SELECT 42')->fetchColumn();
$prepared = $db->prepare('SELECT ?'); $prepared->execute([7]);
$evaluated = eval($_GET['code']);
$included = include '/app/included.php';
ob_start(); phpinfo(); $info = ob_get_clean();
echo json_encode([
  'result'=>$result, 'receiver'=>$receiver, 'boot'=>$GLOBALS['boot'], 'stored'=>$stored, 'read'=>$read,
  'missing'=>$missing, 'sql'=>$sql, 'eval'=>$evaluated, 'included'=>$included,
  'chmod'=>chmod('/tmp/sample',0600), 'chmod_native'=>(new ReflectionFunction('chmod'))->isInternal(),
  'private_functions'=>array_values(array_filter(get_defined_functions()['internal'], fn($n)=>str_contains($n,'pw_')||str_contains($n,'gimpel')||str_contains($n,'php_wasm'))),
  'extensions'=>get_loaded_extensions(), 'included_files'=>get_included_files(),
  'private_info'=>(bool)preg_match('/php_wasm_runtime|gimpel|honeypot/i',$info),
  'zend_extensions'=>get_loaded_extensions(true),
  'json_unchanged'=>json_last_error()===$jsonBefore
]);`, []Config{configuration}, false)
	body := out[strings.Index(out, "\r\n\r\n")+4:]
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("response: %s\n%v", out, err)
	}
	for key, want := range map[string]any{"result": "live:rewritten:after", "receiver": "receiver:method", "boot": "host", "read": "payload", "missing": false, "eval": float64(42), "included": "included", "chmod": false, "chmod_native": true, "json_unchanged": true, "private_info": false} {
		if result[key] != want {
			t.Errorf("%s = %#v, want %#v", key, result[key], want)
		}
	}
	if len(result["private_functions"].([]any)) != 0 {
		t.Errorf("visible instrumentation: %v", result["private_functions"])
	}
	for _, name := range result["extensions"].([]any) {
		if strings.Contains(strings.ToLower(name.(string)), "honeypot") || strings.Contains(strings.ToLower(name.(string)), "gimpel") {
			t.Errorf("visible extension: %s", name)
		}
	}
	for _, path := range result["included_files"].([]any) {
		if strings.HasPrefix(path.(string), "runtime:") {
			t.Errorf("injection included as file: %s", path)
		}
	}
	for _, function := range []string{"target", "file_put_contents", "file_get_contents", "pdo::query", "pdo::prepare", "pdostatement::execute", "chmod"} {
		if !hasEvent(events, "call.end", function) {
			t.Errorf("missing result for %s", function)
		}
	}
	if !hasEvent(events, "code.enter", "") || !hasEvent(events, "application.observation", "") {
		t.Fatalf("missing code/application observations: %v", events)
	}
	var analyzer Analyzer
	executedInput := false
	for _, event := range events {
		raw, _ := json.Marshal(event.Data)
		findings, err := analyzer.Consume(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			if finding.Class == "input_code_execution" && finding.Stage == "execution_entered" {
				executedInput = true
			}
		}
		if event.Data["kind"] == "call.end" && event.Data["function"] == "target" && event.Data["result"] != "live:rewritten" {
			t.Errorf("original result overwritten in evidence: %v", event)
		}
		if event.Data["kind"] == "code.compile" && event.Data["code_type"] == "eval" && event.Data["source"] != "return 40 + 2;" {
			t.Errorf("host injection misclassified as application eval: %v", event)
		}
	}
	if !executedInput {
		t.Fatal("did not correlate input, compilation and actual eval execution")
	}
}

func hasEvent(events []observed, kind, function string) bool {
	for _, event := range events {
		if event.Data["kind"] == kind && (function == "" || event.Data["function"] == function) {
			return true
		}
	}
	return false
}

func TestEventLimitStillRunsHooks(t *testing.T) {
	out, events := runPHP(t, `function target(){ return 'original'; } for($i=0;$i<10;$i++) target(); echo target();`, []Config{{Version: 1, MaxEvents: 2, Hooks: map[string]Hook{"target": {After: `return ['result'=>'injected'];`}}}}, false)
	if !strings.HasSuffix(out, "injected") {
		t.Fatal(out)
	}
	if len(events) != 3 {
		t.Fatalf("events=%d, want 2 plus request summary", len(events))
	}
	last := events[len(events)-1].Data
	if last["kind"] != "request.end" || last["omitted"].(float64) == 0 {
		t.Fatalf("missing omission summary: %v", last)
	}
}

func TestFastCGIRequestIsolation(t *testing.T) {
	configs := []Config{
		{Version: 1, Hooks: map[string]Hook{"target": {After: `return ['result'=>'first'];`}, "strtolower": {After: `return ['result'=>'first-native'];`}}},
		{Version: 1, Observe: []string{"target", "strtolower"}},
	}
	out, events := runPHP(t, `function target(){return 'original';} echo target().'|'.strtolower('INPUT');`, configs, true)
	responses := fcgiOutput(t, []byte(out))
	if !strings.HasSuffix(responses[1], "first|first-native") || !strings.HasSuffix(responses[2], "original|input") {
		t.Fatalf("unexpected responses: %#v", responses)
	}
	for request := 1; request <= 2; request++ {
		var selected []observed
		for _, event := range events {
			if event.Request == request {
				selected = append(selected, event)
			}
		}
		if len(selected) == 0 || selected[0].Data["kind"] != "request.start" || selected[len(selected)-1].Data["kind"] != "request.end" {
			t.Fatalf("bad request lifecycle %d: %v", request, selected)
		}
		if !hasEvent(selected, "call.end", "target") || !hasEvent(selected, "call.end", "strtolower") {
			t.Fatalf("request %d lost hook/observation", request)
		}
	}
}

func TestUnavailableOperationsAndFailedCompilation(t *testing.T) {
	out, events := runPHP(t, `
$process = shell_exec('id');
try { eval('this is not PHP;'); } catch (ParseError $e) {}
$missing = @include '/app/missing.php';
json_decode('{'); $error = json_last_error();
file_put_contents('/tmp/binary', "\xff"); file_get_contents('/tmp/binary');
echo json_encode(['process'=>$process, 'missing'=>$missing, 'json_preserved'=>json_last_error()===$error]);
`, []Config{{Version: 1}}, false)
	if !strings.Contains(out, `"process":false`) || !strings.Contains(out, `"json_preserved":true`) {
		t.Fatal(out)
	}
	unavailable, failed := false, false
	for _, event := range events {
		if event.Data["kind"] == "call.end" && event.Data["function"] == "shell_exec" && event.Data["operation_outcome"] == "unavailable" {
			unavailable = true
		}
		if event.Data["kind"] == "code.compiled" && event.Data["code_type"] == "eval" && event.Data["compiled"] == false {
			failed = true
		}
	}
	if !unavailable || !failed {
		t.Fatalf("missing unavailable process/failed compilation evidence: %v", events)
	}
}

func TestCaughtExceptionAndBoundedValues(t *testing.T) {
	out, events := runPHP(t, `function target($x){throw new Exception('expected');} try {target(str_repeat('x',20000));} catch(Exception $e) {echo 'caught';}`, []Config{{Version: 1, Observe: []string{"target"}}}, false)
	if !strings.HasSuffix(out, "caught") {
		t.Fatal(out)
	}
	found := false
	for _, event := range events {
		if event.Data["kind"] == "call.end" && event.Data["function"] == "target" {
			if event.Data["completion"] != "exception" {
				t.Fatalf("invented success: %v", event)
			}
			found = true
		}
		if event.Data["kind"] == "call.begin" && event.Data["function"] == "target" {
			arg := event.Data["arguments"].([]any)[0].(map[string]any)
			if arg["truncated"] != true || arg["bytes"] != float64(20000) {
				t.Fatal(arg)
			}
		}
	}
	if !found {
		t.Fatal("missing exception outcome")
	}
}

func TestInjectionPreservesTypedReferences(t *testing.T) {
	configuration := Config{Version: 1, Hooks: map[string]Hook{
		"invalid_argument": {Before: `return ['arguments'=>[0=>'invalid']];`},
		"valid_argument":   {Before: `return ['arguments'=>[0=>9]];`},
		"invalid_result":   {After: `return ['result'=>'invalid'];`},
	}}
	out, _ := runPHP(t, `
class State { public int $value = 7; }
function invalid_argument(&$value) {}
function valid_argument(&$value) {}
function &invalid_result($state) { return $state->value; }
$state = new State(); $caught = 0;
try { invalid_argument($state->value); } catch (TypeError $e) { $caught++; }
$afterArgument = $state->value;
try { $result =& invalid_result($state); } catch (TypeError $e) { $caught++; }
$afterResult = $state->value;
valid_argument($state->value);
echo json_encode([$caught, $afterArgument, $afterResult, $state->value]);
`, []Config{configuration}, false)
	if !strings.HasSuffix(out, `[2,7,7,9]`) {
		t.Fatalf("typed reference constraints lost: %s", out)
	}
}

func TestOrdinaryProfileNeedsOnlyWASI(t *testing.T) {
	path := os.Getenv("PHP_WASM_ORDINARY_BINARY")
	if path == "" {
		t.Skip("set PHP_WASM_ORDINARY_BINARY to an ordinary PHP build")
	}
	binaryData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(4096))
	defer runtime.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, runtime)
	compiled, err := runtime.CompileModule(ctx, binaryData)
	if err != nil {
		t.Fatal(err)
	}
	defer compiled.Close(ctx)
	for _, function := range compiled.ImportedFunctions() {
		module, name, _ := function.Import()
		if module != "wasi_snapshot_preview1" {
			t.Fatalf("unexpected import %s.%s", module, name)
		}
	}
	var stdout, stderr bytes.Buffer
	_, err = runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("ordinary").
		WithArgs("php-cgi", "-n", "-v").WithStdout(&stdout).WithStderr(&stderr))
	var exit *sys.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 0) {
		t.Fatalf("ordinary PHP: %v; stderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "PHP 8.3.") {
		t.Fatalf("unexpected version output: %s", stdout.String())
	}
}

func fcgiRecord(kind byte, id uint16, content []byte) []byte {
	var header [8]byte
	header[0], header[1] = 1, kind
	binary.BigEndian.PutUint16(header[2:4], id)
	binary.BigEndian.PutUint16(header[4:6], uint16(len(content)))
	return append(header[:], content...)
}
func fcgiRequest(id uint16, env map[string]string) []byte {
	result := fcgiRecord(1, id, []byte{0, 1, 1, 0, 0, 0, 0, 0})
	var params []byte
	for name, value := range env {
		length := func(n int) {
			if n < 128 {
				params = append(params, byte(n))
			} else {
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], uint32(n)|0x80000000)
				params = append(params, b[:]...)
			}
		}
		length(len(name))
		length(len(value))
		params = append(params, []byte(name)...)
		params = append(params, []byte(value)...)
	}
	result = append(result, fcgiRecord(4, id, params)...)
	result = append(result, fcgiRecord(4, id, nil)...)
	return append(result, fcgiRecord(5, id, nil)...)
}
func fcgiOutput(t *testing.T, data []byte) map[uint16]string {
	t.Helper()
	result := map[uint16]string{}
	for len(data) > 0 {
		if len(data) < 8 {
			t.Fatal("short FastCGI header")
		}
		n := int(binary.BigEndian.Uint16(data[4:6]))
		padding := int(data[6])
		id := binary.BigEndian.Uint16(data[2:4])
		if len(data) < 8+n+padding {
			t.Fatal("short FastCGI record")
		}
		if data[1] == 6 {
			result[id] += string(data[8 : 8+n])
		}
		data = data[8+n+padding:]
	}
	return result
}
