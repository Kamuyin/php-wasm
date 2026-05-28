# WordPress on WASI PHP

WordPress runs on php-wasm using the official
[sqlite-database-integration](https://wordpress.org/plugins/sqlite-database-integration/)
plugin as a `wp-content/db.php` drop-in. No MySQL, no network connection.

## What works / what doesn't

| Feature | Status |
|---------|--------|
| WordPress core (posts, pages, themes, settings) | Works |
| SQLite via drop-in plugin | Works |
| PHP extensions (mbstring, xml, dom, simplexml, fileinfo, exif) | Works |
| Sessions (`/tmp` persistence across requests) | Works |
| Permalink routing | Works (Go host rewrites to index.php) |
| File uploads (media library) | Works (stored in `data/uploads/`) |
| wp-admin interface | Works |
| Plugin/theme installation via wp-admin | Blocked — no outbound HTTP in WASI Preview1 |
| WordPress auto-updates | Disabled (`AUTOMATIC_UPDATER_DISABLED=true`) |
| Remote wp-cron | Disabled (see [Cron](#cron) below) |
| Image resizing / thumbnails | Not available — gd not in `wordpress` profile v1 |
| MySQL / Postgres | Not available — no sockets in WASI Preview1 |
| HTTPS / TLS | No — put a reverse proxy (nginx, Caddy) in front |

## Quick start

### 1. Build the WASM binary

```bash
scripts/build-libxml2.sh
scripts/build-sqlite3.sh
make build PROFILE=wordpress VERSION=8.3
# Output: out/php-8.3.30-wordpress.wasm
```

Or with Docker:

```bash
make docker-build PROFILE=wordpress VERSION=8.3
```

### 2. Bootstrap WordPress

```bash
cd examples/wazero-wordpress
./setup.sh
```

`setup.sh` downloads WordPress 6.6.2 and the sqlite-database-integration plugin,
installs `wp-content/db.php`, and generates `wp-config.php` with random salts.
It is idempotent — safe to run multiple times.

```bash
WP_VERSION=6.7.1 ./setup.sh
WP_TARBALL_PATH=/path/to/wordpress.tar.gz ./setup.sh
```

### 3. Start the server

```bash
go run . --wasm ../../out/php-8.3.30-wordpress.wasm
```

Open `http://localhost:8080/wp-admin/install.php` to run the installer.

## Directory layout

```
examples/wazero-wordpress/
├── main.go       Go HTTP server (compile WASM once, instantiate per request)
├── setup.sh      Bootstrap script
├── php.ini       PHP config mounted at /etc/php inside WASM
├── www/          WordPress files (created by setup.sh, gitignored)
└── data/
    ├── database/ SQLite DB (.ht.sqlite written here by the plugin)
    └── uploads/  wp-content/uploads (media library)
```

## Filesystem mounts (per request)

| Host path        | WASI path                      | Access |
|------------------|--------------------------------|--------|
| `www/`           | `/srv/app`                     | rw     |
| `data/database/` | `/srv/app/wp-content/database` | rw     |
| `data/uploads/`  | `/srv/app/wp-content/uploads`  | rw     |
| `/tmp`           | `/tmp`                         | rw     |
| dir of `php.ini` | `/etc/php`                     | ro     |

## Build profile: `wordpress`

| Extension | Included | Reason |
|-----------|----------|--------|
| mbstring | Yes | WP requires multi-byte string support |
| pdo_sqlite + sqlite3 | Yes | Core of the SQLite database drop-in |
| xml / dom / simplexml / xmlreader / xmlwriter | Yes | RSS, oEmbed, importer, REST API |
| fileinfo | Yes | MIME type detection for uploads |
| exif | Yes | Media library metadata |
| gd | No | Adds ~3 MB + 3 native dep builds; WP works without (no thumbnails) |
| zip | No | Needed for plugin/theme upload — blocked anyway in WASI |
| openssl | No | No network in WASI; misleading to include |

Expected binary size: ~7–8 MB.

## wp-config.php settings

`setup.sh` generates `www/wp-config.php` with these WASI-specific constants:

| Constant | Value | Why |
|----------|-------|-----|
| `DISABLE_WP_CRON` | `true` | No outbound HTTP; use a host-side cron |
| `AUTOMATIC_UPDATER_DISABLED` | `true` | No network for update checks |
| `WP_HTTP_BLOCK_EXTERNAL` | `true` | Prevents WP from attempting HTTP requests |
| `FS_METHOD` | `'direct'` | No FTP/SSH; WordPress writes files directly |
| `WP_HOME` / `WP_SITEURL` | `http://localhost:8080` | Update for production |

## Cron

Run WordPress background tasks from the host:

```bash
* * * * * curl -s "http://localhost:8080/wp-cron.php?doing_wp_cron" >/dev/null 2>&1
```

## Server options

```
--wasm      Path to php-cgi .wasm binary  (default: ../../out/php-8.3.30-wordpress.wasm)
--docroot   WordPress document root       (default: ./www)
--data-dir  Persistent data directory     (default: ./data)
--addr      HTTP listen address           (default: :8080)
--timeout   Per-request timeout           (default: 60s)
--php-ini   Path to php.ini              (default: ./php.ini)
```

## Persistence and backup

```bash
cp data/database/.ht.sqlite database-backup-$(date +%Y%m%d).sqlite
tar czf uploads-backup-$(date +%Y%m%d).tar.gz data/uploads/
```

## Troubleshooting

**White screen / no output:** Check stderr. Run with `display_errors = On` in php.ini.

**"Your server does not have the xml extension":** Use the `wordpress` profile — `default` and `minimal` don't include xml.

**"Could not connect to database":** Verify `data/database/` is writable and mounted. Check Go server stderr for mount errors.

**Redirects loop on wp-admin:** `WP_HOME` and `WP_SITEURL` must match the address you're browsing to.

## Roadmap

- Image resizing: add gd once libjpeg/libpng build scripts are stable.
- ZIP extension: enables plugin/theme upload via wp-admin.
- MySQL via host-import: see [MYSQL.md](MYSQL.md) for the planned proxy architecture.
