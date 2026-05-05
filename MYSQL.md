Good — here's the concrete implementation plan. There are **4 components** that need to work together.

---

## Architecture

The WASM module never touches a real socket. Instead, it calls host-imported functions via WASM's import mechanism. The host (wazero) owns the MySQL connection pool and proxies at the **protocol level** — WASM sends raw MySQL wire protocol bytes, the host reads/writes them to a real MySQL server.

```
PHP (WASM)
  └─ mysqlnd
      └─ socket() / connect() / send() / recv()
          └─ WASI shim (patch) → WASM imports
                                    ↓
                              wazero host module
                                    ↓
                              real MySQL server
```

---

## Component 1: WASI Socket Shim Patch

A new patch (`patches/common/0050-wasi-socket-proxy.patch`) intercepts POSIX socket calls that the existing WASI patches stub out with errors. Instead of erroring, we redirect through WASM imports.

The patch adds a new file `wasi/sockets/socket_proxy.c`:

```c
// WASM host imports — these come from the wazero host module
__attribute__((import_module("php_net"), import_name("connect")))
extern int32_t host_connect(int32_t host_ptr, int32_t host_len,
                             int32_t port,
                             int32_t user_ptr, int32_t user_len,
                             int32_t pass_ptr, int32_t pass_len,
                             int32_t db_ptr,   int32_t db_len);

__attribute__((import_module("php_net"), import_name("send")))
extern int32_t host_send(int32_t conn_id, int32_t buf_ptr, int32_t len);

__attribute__((import_module("php_net"), import_name("recv")))
extern int32_t host_recv(int32_t conn_id, int32_t buf_ptr, int32_t max_len);

__attribute__((import_module("php_net"), import_name("close")))
extern void host_close(int32_t conn_id);

// Virtual fd table (fds 1000–1099 → conn_id 0–99)
#define VIRT_FD_BASE 1000
#define VIRT_FD_MAX  100
static int32_t virt_conn[VIRT_FD_MAX];  // conn_id or -1

// Called from patched socket() stub
int wasi_socket_create(void) {
    for (int i = 0; i < VIRT_FD_MAX; i++) {
        if (virt_conn[i] == -1) {
            virt_conn[i] = -2; // reserved
            return VIRT_FD_BASE + i;
        }
    }
    errno = EMFILE;
    return -1;
}

// Called from patched connect() stub — this is where MySQL DSN is parsed from env
int wasi_socket_connect(int fd, const char *host, int port) {
    int slot = fd - VIRT_FD_BASE;
    // Credentials injected via WASI env vars, not from PHP (avoids tenant spoofing)
    const char *user = getenv("MYSQL_USER");
    const char *pass = getenv("MYSQL_PASS");
    const char *db   = getenv("MYSQL_DB");
    int32_t cid = host_connect(
        (int32_t)host, strlen(host), port,
        (int32_t)user, strlen(user),
        (int32_t)pass, strlen(pass),
        (int32_t)db,   strlen(db)
    );
    if (cid < 0) { errno = ECONNREFUSED; return -1; }
    virt_conn[slot] = cid;
    return 0;
}

ssize_t wasi_socket_send(int fd, const void *buf, size_t len) {
    return host_send(virt_conn[fd - VIRT_FD_BASE], (int32_t)buf, len);
}

ssize_t wasi_socket_recv(int fd, void *buf, size_t len) {
    return host_recv(virt_conn[fd - VIRT_FD_BASE], (int32_t)buf, len);
}

void wasi_socket_close(int fd) {
    int slot = fd - VIRT_FD_BASE;
    host_close(virt_conn[slot]);
    virt_conn[slot] = -1;
}
```

The existing `socket()`, `connect()`, `read()`, `write()`, `close()` stubs in the WASI initial port patch get a `#ifdef __wasi__` fork that calls these helpers when the fd is in the virtual range.

---

## Component 2: Build System — Profile & Version Changes

**`profiles/default.yaml`** — add MySQL:
```yaml
configure_flags:
  - "--enable-mysqlnd"
  - "--enable-pdo"
  - "--enable-pdo-mysql"
  - ...existing flags...
```

**`versions/8.3/config.yaml`** — list the new patch:
```yaml
common_patches:
  - "0050-wasi-socket-proxy"
version_patches:
  - "0001-wasi-initial-port"
  - "0002-83-wasi-missing-stubs"
```

**`scripts/configure-php.sh`** — add one CFLAGS tweak:
```bash
# Allow virtual fd range — don't stub these out as errors in mysqlnd
CFLAGS="$CFLAGS -DWASI_SOCKET_PROXY=1"
```

---

## Component 3: wazero Host Module

New file `examples/wazero-cgi-minimal/db_proxy.go`:

```go
package main

import (
    "context"
    "database/sql"
    "sync"
    _ "github.com/go-sql-driver/mysql"
    "github.com/tetratelabs/wazero/api"
)

type dbProxy struct {
    mu    sync.Mutex
    conns map[int32]*sql.Conn
    next  int32
    pool  *sql.DB
}

func (p *dbProxy) register(ctx context.Context, rt wazero.Runtime) error {
    _, err := rt.NewHostModuleBuilder("php_net").
        NewFunctionBuilder().
        WithGoModuleFunction(api.GoModuleFunc(p.connect),
            // host_ptr, host_len, port, user_ptr, user_len, pass_ptr, pass_len, db_ptr, db_len
            []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32,
                            api.ValueTypeI32, api.ValueTypeI32,
                            api.ValueTypeI32, api.ValueTypeI32,
                            api.ValueTypeI32, api.ValueTypeI32},
            []api.ValueType{api.ValueTypeI32}).Export("connect").
        NewFunctionBuilder().
        WithGoModuleFunction(api.GoModuleFunc(p.send),
            []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
            []api.ValueType{api.ValueTypeI32}).Export("send").
        NewFunctionBuilder().
        WithGoModuleFunction(api.GoModuleFunc(p.recv),
            []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
            []api.ValueType{api.ValueTypeI32}).Export("recv").
        NewFunctionBuilder().
        WithGoModuleFunction(api.GoModuleFunc(p.close),
            []api.ValueType{api.ValueTypeI32}, nil).Export("close").
        Instantiate(ctx)
    return err
}

func (p *dbProxy) connect(ctx context.Context, mod api.Module, params []uint64) []uint64 {
    mem := mod.Memory()
    host, _ := mem.Read(uint32(params[0]), uint32(params[1]))
    // NOTE: credentials come from WASI env (set by host), not from WASM params
    // params[2] is port, used to find the right pool if you have multiple DBs
    conn, err := p.pool.Conn(ctx)
    if err != nil { return []uint64{uint64(0xFFFFFFFF)} }
    p.mu.Lock()
    id := p.next; p.next++
    p.conns[id] = conn
    p.mu.Unlock()
    _ = host // ignored — connection always goes to the pre-configured pool
    return []uint64{uint64(id)}
}

// send/recv implement the MySQL wire protocol relay via conn.Raw()
// The conn.Raw() callback gets the underlying *mysql.mysqlConn's net.Conn
// and we copy bytes directly between WASM memory and the real MySQL socket.
```

---

## Component 4: Security boundary — credential injection

The key isolation property: **PHP never sees the MySQL password**. Instead of `new PDO("mysql:host=...;dbname=...", $user, $pass)` working with real credentials, the host:

1. Sets `MYSQL_USER`, `MYSQL_PASS`, `MYSQL_DB` in the WASM module's env at instantiation time (per-tenant values from the host's auth store)
2. The shim ignores whatever credentials PHP passes to `connect()` and reads from env instead
3. Each WASM module instance gets its own connection slot — isolation is enforced at the wazero module boundary

---

## What would need to be written

| File | Status |
|---|---|
| `patches/common/0050-wasi-socket-proxy.patch` | New — largest piece |
| `profiles/default.yaml` | Small edit |
| `versions/{8.2,8.3,8.4}/config.yaml` | Small edit each |
| `scripts/configure-php.sh` | One line |
| `examples/wazero-cgi-minimal/db_proxy.go` | New — Go host module |
| `tests/smoke/06-mysql-proxy.php` | New — smoke test |

Want me to start implementing this, beginning with the patch file and the wazero host module?
