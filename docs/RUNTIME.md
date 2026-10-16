# Native runtime observation and injection

The PHP 8.3 `honeypot` profile adds a private runtime interface for an embedding
host. It records selected PHP operations and accepts host-provided PHP scripts
for application observation and compatibility. Supported operations execute
inside the existing WASI sandbox. This interface neither adds host networking
nor launches host processes.

The implementation is compiled into PHP core. It registers no instrumentation
extension, PHP function, class, constant, INI directive, stream wrapper, or
WordPress plugin. The host configuration and event destination are not supplied
through PHP environment variables or application files. The normal profiles
retain their existing WASI-only import contract.

## Build and run

With the WASI SDK and the existing SQLite/libxml2 dependencies installed:

```bash
scripts/build.sh 8.3 honeypot --no-wasm-opt --jobs 8
```

The resulting `out/php-8.3.30-honeypot.wasm` requires the runtime host imports.
It is not a drop-in replacement for a consumer that only links WASI. PHP 8.2 and
8.4 instrumented builds are rejected until their core integration is ported.

The reference consumer uses wazero and runs one CGI request:

```bash
cd examples/runtime-observer
go run . --script /path/to/application/index.php \
  --policy /path/outside/application/runtime.json \
  --events /path/outside/application/observations.jsonl \
  --data /path/to/instance-data --query 'example=value'
```

The application directory is mounted read-only at `/app`, a fresh temporary
directory at `/tmp`, and optional persistent instance storage at `/data`.
CGI output goes to stdout. The host creates the event file exclusively with mode
0600 and attaches its own random request identifier. The example enforces a
30-second timeout, a 256 MiB WASM memory limit, and a 32 MiB response limit.
Keep the policy and output outside every guest mount, including aliases and
symlinks. Production consumers must scope persistent storage to each instance.

## Host interface

The additional WASM import namespace is `php_wasm_runtime_v1`:

| Import | Signature | Contract |
| --- | --- | --- |
| `config` | `(buffer: i32, capacity: i32) -> i32` | Write one UTF-8 JSON configuration; return its byte length, or a negative error |
| `emit` | `(buffer: i32, length: i32) -> i32` | Copy one JSON event before returning; return zero on success, nonzero on loss |
| `database` | `(request: i32, length: i32, response: i32, capacity: i32) -> i32` | Copy one JSON request to the private database worker; return response bytes, or negative on error |

`config` is called after PHP request initialization and before the entrypoint.
Its capacity is 262144 bytes. It is called again for every FastCGI request;
configuration, scripts, counters, and correlation maps are discarded at request
shutdown. Invalid configuration aborts the request. Missing imports prevent WASM
instantiation. Neither case silently runs an uninstrumented request.

`runtime/host` provides a reusable Go adapter with configuration validation,
guest-memory bounds checks, event copying, and callbacks for storage/errors.
Use one `Analyzer` per module, consume events in order, and assign identity and
request correlation in the embedding host. Event contents are untrusted evidence;
an application observation is never authority to change host policy or identity.

### Configuration

```json
{
  "version": 1,
  "max_events": 512,
  "observe": ["wp_authenticate", "example\\service::login"],
  "hooks": {
    "legacy_format": {
      "before": "return ['arguments' => [0 => trim($arguments[0])]];",
      "after": "return ['result' => $result, 'emit' => ['name' => 'legacy_format.used']];"
    }
  }
}
```

Function names use lowercase, with `class::method` for methods. Up to 128 explicit
names and 128 hooks are accepted. A configured hook also enables observation for
that function. Optional `bootstrap` contains PHP statements to run once before
the application's entrypoint. It can define compatibility functions or register
later behavior; application frameworks have not necessarily loaded at that point.

Only the embedding host supplies this configuration. Gimpel loads it from a
verified `component.runtime.host_artifact` asset bundle outside guest preopens.
It is never supplied through master configuration substitutions or HTTP requests. Script code is executable
content and must follow the same repository trust boundary as application code.

### Injected PHP scripts

Scripts contain PHP statements without opening/closing tags. Each is compiled as
an unnamed closure with four local parameters:

| Parameter | Meaning |
| --- | --- |
| `$arguments` | Copy of the call arguments; contained objects retain their live identity |
| `$result` | Copy of the original result for an `after` hook; null before the call |
| `$object` | The live receiver object for an instance method, otherwise null |
| `$event` | Bounded observation metadata for this call |

A script can read application globals, call loaded application functions, and
modify live objects. It can return an array containing:

- `arguments`: zero-based argument replacements, applied by a `before` hook.
  The hook cannot add arguments or skip the original call. References bound to
  typed properties retain PHP's type constraints; incompatible assignments throw.
- `result`: replacement result, applied by an `after` hook. It does not undo
  side effects of the original function. The operator is responsible for a
  replacement compatible with the caller's expectations.
- `emit`: a custom observation payload, subject to normal snapshot/event limits.

Before/after events retain the original call input/result; `operator_hook: true`
marks hooked calls. The example analyzer excludes these calls from automatic
exploit correlation. Operations executed synchronously by an injected script are
excluded from the observation hooks to prevent recursion and self-attribution.
They remain subject to WASI access and host resource limits.

Script failures propagate as application errors. The error observer marks
`injection: true` when the error occurs during a hook. Fatal WASI traps may prevent
normal PHP shutdown; the host must record a runtime failure and incomplete trace.
An event limit does not disable compatibility hooks.

The native interface is absent from `get_loaded_extensions()`,
`get_defined_functions()`, and INI inspection. Injected scripts execute in the
application: functions, globals, WordPress callbacks, output, and other effects
they create can be discovered. Script frames can appear as
`runtime://host-script`. This is the accepted visibility tradeoff for access to
live application objects. Generic WASI/platform fingerprints and native memory
corruption are outside this concealment guarantee.

## Observation coverage

| Events | Evidence |
| --- | --- |
| `request.start`, `request.end` | Bounded parsed GET/POST values; event/loss counts and response status |
| `call.begin`, `call.end` | Selected function/method, arguments, receiver identity, source location, original return value or exception |
| `code.compile`, `code.compiled` | File/include or eval compilation attempt and compilation result |
| `code.enter`, `code.leave` | Actual entry/exit of top-level file/eval code, linked by `compile_id` where available |
| `runtime.error` | PHP warnings/errors, source location, and injection context |
| `application.observation` | Explicit payload returned by an operator script |
| `configuration.error` | Invalid host configuration before application execution |
| `security.evidence` | Structural SQLi evidence or a database compatibility rejection |

Default call selection covers file open/read/write/delete/rename/copy/upload,
process APIs, socket/HTTP/mail APIs, deserialization, and common PDO/SQLite/
mysqli/PostgreSQL query/prepare/execute/binding functions. A function must exist
in the selected PHP build to be called. Missing APIs produce PHP errors rather
than invented successful operations. Native mysqli, pgsql and PDO mysql/pgsql facades use the private database
worker described in [MYSQL.md](MYSQL.md).

`chmod()` is restored as a native `standard` function with its normal signature.
WASI Preview1 cannot apply permissions: it returns false and records
`operation_outcome: unavailable`. Process calls likewise remain unavailable.
This preserves a normal failure path without installing a PHP compatibility file.

Snapshots do not invoke application serializers, string conversions, property
getters, or iterators. Objects/resources are represented by request-local identity
and class/type; an explicit script can inspect live objects when needed. Strings
are limited to 16 KiB, arrays to 32 entries per level, depth to four, and traversal
to 128 values. Truncation is explicit. Invalid UTF-8 is replaced during JSON
encoding; retain the original HTTP evidence when byte-exact payloads are needed.
The transport limits each event to 64 KiB and records omissions if it cannot fit.
`json_last_error()` is preserved around native encoding/decoding.

At most 512 ordinary events are emitted per request, configurable downward.
Separate reserves admit 64 application observations and 128 security/provenance
events, so ordinary WordPress includes cannot starve authentication outcomes;
`request.end` is an additional summary even when that budget is exhausted. Long
requests can therefore have incomplete traces. The host also needs its own byte,
rate, and storage limits. Production collectors should retain valuable application
outcomes and suspicious operation evidence rather than forward every ordinary
file include or query into the central event service. The reference CLI saves
the raw trace to support development of those policies.

## Attempts and observed effects

The example `Analyzer` correlates parsed input values with operations. It emits
heuristic SQL/command/path input signals, records when matching bytes reach a
relevant argument, and distinguishes returned, failed, unavailable, and merely
prepared operations. An exact input value compiled as PHP can be followed through
compilation to actual code entry and return. A parser error is never execution.

Native provenance now tracks request-derived **Zend string identities**, with
byte ranges preserved through concatenation (`.`, `.=`), interpolated strings,
and copies. GET, POST, cookies, HTTP headers/auth fields and request URI/query
are seeded independently. Whole-input transformations include trim, case changes,
URL/base64 decoding, substr, addslashes and stripslashes. `file_get_contents`
on `php://input` marks a raw body; `json_decode` propagates wholly derived JSON
to decoded string values. Mixed-source formatting, general replacement/filtering,
stream reads, uploads, serialization, persistence and later database reads are
not fully tracked. Unknown transformations lose provenance rather than infer it
from matching text. A fixed string equal to an input is not marked.

Provenance retains at most 4 MiB/4096 string identities and 64 ranges per string;
`request.end.provenance_limited` reports exhaustion. Values are held by reference
to prevent address reuse and preserve PHP copy-on-write behavior. No application
serializer or getter is invoked. Trace quotas and incomplete provenance can
produce false negatives; they never imply success.

The reference analyzer confirms request-derived executable PHP syntax only after
code entry. Its conservative lexical check excludes bytes confined to comments,
quoted strings and unsupported heredoc forms. The database worker confirms the
structural SQLi case in [MYSQL.md](MYSQL.md); query success alone is insufficient.
Gimpel additionally confirms a successful request-derived read of `/app/wp-config.php`
or `/data/wordpress-secrets.json`, or entry into request-selected PHP in protected
configuration/writable uploads. These are explicit WordPress boundaries. Ordinary
public upload reads are not confirmed disclosures. A read verdict describes data
access; it does not claim that bytes were exfiltrated in an HTTP response.

SQL/command/path patterns remain unconfirmed attempts. Process and unavailable
filesystem/network operations retain unavailable outcomes. Missing return events,
parse failures, traps and collection limits are never treated as successful
exploitation. These rules cover defined behaviors, not every vulnerability class.

## Gimpel integration

The FastCGI host links private imports only for an admitted, verified host bundle.
It assigns agent/module/instance/request identity, copies guest events under
separate trace/security byte budgets, and forwards application outcomes, threat
evidence and loss summaries. Raw include/query traces stay out of the gateway.
The WordPress module's host scripts preserve authentication, account, upload,
plugin/theme and outbound-attempt reporting without registering MU plugins or
WordPress callbacks. The database interpreter and files are outside every app
mount. A fresh installation is expected when upgrading from the old SQLite
plugin module; no legacy database migration is performed.

Useful later capabilities include content hashes for dynamically executed code,
uploaded-file-to-execution correlation, outbound/SSRF attempt evidence, object
deserialization chains, response-linked exfiltration evidence, and replayable
request traces with instance snapshots. Each needs explicit collection budgets
and evidence semantics.

## Acceptance checks

```bash
cd runtime/host
PHP_WASM_BINARY="$PWD/../../out/php-8.3.30-honeypot.wasm" \
  PHP_WASM_ORDINARY_BINARY="$PWD/../../out/php-8.3.30-wordpress.wasm" \
  PHP_WASM_DATABASE_BACKEND=/tmp/php-wasm-database \
  go test ./... -v -count=1 -timeout=5m
```

Prepare `/tmp/php-wasm-database` with `runtime/database/prepare.sh` first.
Without `PHP_WASM_BINARY`, native acceptance cases skip and configuration/evidence
unit cases still run. The native cases cover absence of instrumentation APIs,
live-object and scalar injection, original outcome retention, file/SQL/eval
observation, compilation versus execution, bounded values, errors, event limits,
typed-property references, and changed hook configuration across consecutive
FastCGI requests. Set `PHP_WASM_ORDINARY_BINARY` to a separately built WordPress
profile to check that it imports only WASI and starts without observation imports;
that check skips when the variable is absent. The native runtime CI workflow builds
both profiles and runs these cases.

Database acceptance also covers native mysqli/PDO/pgsql APIs, persistence,
transactions, prepared/escaped negative SQLi cases and confirmed structural SQLi.
The host must link `database` even when disabled; returning -1 explicitly reports
unavailability. JSON wire strings are base64 encoded as `{"bytes":"..."}` and
SQL as `sql_b64`; request/response caps are 1/4 MiB. See the worker and native
facade for the version-1 connect/close/reset/prepare/query/quote envelopes.
