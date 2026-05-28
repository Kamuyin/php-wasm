# wazero-wordpress

Runs WordPress on WASI PHP with SQLite. See [docs/WORDPRESS.md](../../docs/WORDPRESS.md) for the full setup guide.

```bash
./setup.sh
go run . --wasm ../../out/php-8.3.30-wordpress.wasm
# open http://localhost:8080/wp-admin/install.php
```
