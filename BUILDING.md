## Build
```
# Build PHP 8.3 with the minimal profile
make build VERSION=8.3 PROFILE=minimal

# Output: out/php-8.3.30-minimal.wasm
```

## Test
```
# Build the wazero test runner once
make test-runner

# Run smoke tests
make test VERSION=8.3 PROFILE=minimal
```

Or directly with the test runner:
```
cd tests/runners/wazero
go run main.go --wasm ../../out/php-8.3.30-minimal.wasm --script ../smoke/01-hello.php
```
