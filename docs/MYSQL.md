# Embedded MySQL and PostgreSQL compatibility

PHP 8.3's `honeypot` and `moodle` profiles expose native `mysqli`, `mysqli_result`,
`mysqli_stmt`, `PgSql\Connection`, `PgSql\Result`, and PDO `mysql`/`pgsql`
drivers. PHP reflection reports internal functions/classes and ordinary driver
names. No translator classes, SQLite WordPress plugin, or `db.php` drop-in are
loaded into the application. Ordinary profiles keep their existing extensions.

## Isolation and storage

```text
application PHP WASM: mysqli / PDO mysql / pgsql / PDO pgsql
  -> private php_wasm_runtime_v1.database import
  -> host-owned, instance-scoped connection service
  -> separate PHP WASM interpreter: dialect translator + PDO SQLite
  -> private persistent database files
```

The worker has read-only `/backend`, private `/data`, and ephemeral `/tmp`.
The application cannot mount or read these directories. No database server,
network connection, shell, host SQL extension, or host database credentials are
involved. Hostname/user/password inputs are facade inputs; they do not select
real endpoints. Database names are 1–64 ASCII letters/digits/underscores and
are hashed into filenames scoped to one instance. A request reset drops all
connections and rolls back pending transactions; committed data survives reset
and process restart. Separate instances have separate storage.

Gimpel uses a separate Wasmtime engine for the worker and each native PHP app,
so epoch cancellation does not interrupt unrelated instances. Worker linear
memory is 128 MiB, reserved from the module's declared total memory allowance;
its PHP heap limit is 96 MiB. An explicit module fuel allowance is divided
between app and worker. Each worker operation has a 10-second deadline.
A timed-out worker is stopped and recreated at the next request boundary.
There are at most 16 simultaneous connections and four database files, each
limited to 16,384 SQLite pages (normally 64 MiB). Queries are limited to 256 KiB,
results to 10,000 rows/3 MiB, requests to 1 MiB, responses to 4 MiB.

## MySQL / MariaDB surface

The worker reuses the independent MySQL lexer/parser and MySQL-on-SQLite layer
from **WordPress SQLite Database Integration 3.0.2**. `runtime/database/prepare.sh`
downloads the exact archive and verifies SHA-256
`1602e75577ad9b3a7e3e4a6a44a81b9541cdee2124d48928faf61c6fd3cd4f74`.
It packages only the independent database library with its GPL license and
upstream provenance. The library runs inside the separate worker interpreter.

Supported paths include connection/init/real_connect, query/real_query,
prepare/execute, `mysqli_stmt::bind_param`/`bind_result`/`get_result`,
`execute_query`, common result fetching/seeking/field metadata, escaping,
charset selection, affected rows, insert IDs, and begin/commit/rollback.
Procedural and object forms are provided for these operations. PDO supports
query, exec, positional/named parameters, bindValue/bindParam, result iteration,
column metadata, quoting and transactions. WordPress 6.6.2 installation, password
and application-password login, XML-RPC, admin, upload and restart persistence
are exercised through Gimpel's actual FastCGI runner.

The upstream layer translates MySQL DDL, indexes, WordPress SQL functions,
schema introspection and common DML. The facade advertises MySQL 8.0.38; it is
not a complete MySQL or MariaDB server. MySQL prepares use lexer-delimited,
typed parameter substitution with the translator's quoting rules. Bound values
never become SQL syntax. PDO therefore reports emulated prepares for `mysql`;
turning emulation off is unsupported. MySQL SQL modes that change quote syntax
(including `NO_BACKSLASH_ESCAPES` and `ANSI_QUOTES`), escaped mode names and
computed SQL-mode assignments are rejected
so parameter escaping and evidence parsing use the same lexical rules.

Explicit `mysqli::multi_query`/`mysqli_multi_query` batches support up to 2,048
statements and 512 KiB total. Each statement retains the 256 KiB cap and the
same SQL policy. Quotes and comments delimit statements; results are consumed
with `store_result`, `more_results` and `next_result`. A failed statement ends
the batch. Pending statements are discarded at request reset. Ordinary
query/prepare calls still reject multiple statements.

The private backend's optional `config.json` may select `mysql_version: 80400`
for Moodle, or `80038` (the default). The upstream parser receives this version;
the PHP facade uses the same connection metadata. Local patches expose plain
MySQL version strings and InnoDB compatibility variables. SQLite remains the
storage engine. `SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED` keeps
SQLite's stronger serializable isolation; it does not enable dirty reads.
See [SQLite isolation](https://www.sqlite.org/isolation.html). These are
compatibility semantics, not full InnoDB or MariaDB equivalence.
The `utf8_bin`, `utf8mb3_bin` and `utf8mb4_bin` collations compare UTF-8 code
points with MySQL's PAD SPACE behavior, including case-sensitive comparisons.
Other collation and case-folding behavior remains limited by the upstream
translator and SQLite; full MySQL Unicode collation equivalence is not claimed.

Unsupported surfaces include async queries,
server-side cursors, persistent PDO connections, replication, stored procedures,
triggers, server administration and complete extension/reflection parity.
Some field metadata and diagnostics are approximations. Unimplemented APIs
are absent or return explicit errors; they must not be used as a success signal.

## PostgreSQL scope

`pg_connect`, query/query_params, prepare/execute, common result fetch and
metadata APIs, escaping, close, and PDO `pgsql` are available. Connections must
be supplied explicitly to procedural calls; implicit default connections and
asynchronous/libpq features are unsupported. `pg_pconnect` is request-scoped.

Supported SQL is the common SQLite-compatible subset: SELECT, joins, basic
DDL/DML, RETURNING, transactions/savepoints, positional `$1` parameters,
PDO `?`/named parameters, `version()` and `current_database()`.
`SERIAL PRIMARY KEY` maps to an SQLite integer primary key. Other SERIAL uses,
PostgreSQL `::` casts, ILIKE, dollar-quoted bodies, arrays, JSONB, enums, schemas,
sequences, COPY, extensions, stored functions and PostgreSQL catalog semantics
are outside this profile. Unsupported syntax fails; it is not advertised as
full PostgreSQL behavior. Basic API coverage is tested without a specific
PostgreSQL application compatibility claim.

## Filesystem controls and evidence

ATTACH/DETACH, PRAGMA, VACUUM, extension loading, filesystem SQL functions,
OUTFILE/DUMPFILE/COPY and SQLite internal objects are rejected by token-aware
policy. Quoted values are not mistaken for keywords. WASI capability isolation
and memory/time limits remain the outer boundary even if a dialect parser fails.

Original SQL carries byte-range provenance from the application interpreter.
The worker can confirm a narrow SQLi class: request-derived bytes close a
literal that began in fixed SQL and introduce executable SQL syntax, and that
statement executes. Parameters are handled separately. Escaped/bound inputs,
preparation alone, syntax failures and unavailable operations are never promoted
to successful SQLi. Native `security.evidence` records preserve the driver,
source, syntax evidence and execution verdict. Compatibility rejections produce
separate `database_compatibility` evidence. Not all database errors are dialect
failures; ordinary missing tables during WordPress installation remain ordinary
query failures.

Stored/second-order SQLi, unquoted numeric injection, ORM-generated mixed
provenance, output-linked data exfiltration and complete dialect semantics are
not claimed by this detector. See [RUNTIME.md](RUNTIME.md) for source coverage.

## Build and check

```bash
scripts/build.sh 8.3 honeypot --no-wasm-opt --jobs 8
scripts/build.sh 8.3 wordpress --no-wasm-opt --jobs 8
runtime/database/prepare.sh /tmp/php-wasm-database
PHP_WASM_BINARY="$PWD/out/php-8.3.30-honeypot.wasm" \
PHP_WASM_ORDINARY_BINARY="$PWD/out/php-8.3.30-wordpress.wasm" \
PHP_WASM_DATABASE_BACKEND=/tmp/php-wasm-database \
  go -C runtime/host test ./... -v -count=1 -timeout=5m
```

`SQLITE_TRANSLATOR_ARCHIVE` supplies the pinned archive for an offline build.
A custom embedding host implements the `Host.Database` callback. It must reset
its worker before returning configuration for every PHP request. The reference
observation CLI intentionally has no database worker; it returns unavailable
for database facade operations. Gimpel supplies the complete worker lifecycle.
