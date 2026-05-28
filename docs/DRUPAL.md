# Drupal on WASI PHP

Drupal runs on php-wasm with SQLite configured in `sites/default/settings.php`.
No MySQL, no outbound network.

## What works / what doesn't

| Feature | Status |
|---------|--------|
| Drupal core install and admin UI | Works |
| SQLite database (`driver=sqlite`) | Works |
| Public file uploads (`sites/default/files`) | Works |
| Private files (`/data/private`) | Works |
| Clean URL routing | Works (Go host rewrites to index.php) |
| Module/theme download via UI | Blocked — no outbound HTTP in WASI Preview1 |
| Automatic update checks/downloads | Blocked — no outbound HTTP |
| MySQL / Postgres | Not available — no sockets in WASI Preview1 |
| HTTPS / TLS termination | No — put a reverse proxy in front |

## Quick start

### 1. Build the WASM binary

```bash
scripts/build-libxml2.sh
scripts/build-sqlite3.sh
make build PROFILE=drupal VERSION=8.3
# Output: out/php-8.3.30-drupal.wasm
```

Or with Docker:

```bash
make docker-build PROFILE=drupal VERSION=8.3
```

### 2. Bootstrap Drupal

```bash
cd examples/wazero-drupal
./setup.sh
```

`setup.sh` downloads a pinned Drupal tarball, prepares `settings.php` for
SQLite, and creates required writable directories. It is idempotent.

```bash
DRUPAL_VERSION=10.3.7 ./setup.sh
DRUPAL_TARBALL_PATH=/path/to/drupal.tar.gz ./setup.sh
```

### 3. Start the server

```bash
go run . --wasm ../../out/php-8.3.30-drupal.wasm
```

Open `http://localhost:8080/core/install.php`.

## Directory layout

```
examples/wazero-drupal/
├── main.go       Go HTTP server
├── setup.sh      Bootstrap script
├── php.ini       PHP runtime config
├── www/          Drupal core (created by setup.sh, gitignored)
└── data/
    ├── database/ drupal.sqlite
    └── private/  Drupal private files
```

## Filesystem mounts

| Host path        | WASI path  | Access |
|------------------|------------|--------|
| `www/`           | `/srv/app` | rw     |
| `data/`          | `/data`    | rw     |
| `/tmp`           | `/tmp`     | rw     |

`settings.php` points SQLite to `/data/database/drupal.sqlite`.

## Notes

- Outbound HTTP-dependent features should be considered unsupported on WASI Preview1.
- If install or admin pages time out, increase `--timeout`.
- Public files live in `www/sites/default/files`; private files in `data/private/`.
