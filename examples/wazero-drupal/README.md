# wazero-drupal

Runs Drupal on WASI PHP with SQLite. See [docs/DRUPAL.md](../../docs/DRUPAL.md) for the full setup guide.

```bash
./setup.sh
go run . --wasm ../../out/php-8.3.30-drupal.wasm
# open http://localhost:8080/core/install.php
```
