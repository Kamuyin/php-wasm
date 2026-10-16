module github.com/php-wasm/examples/runtime-observer

go 1.22

require (
	github.com/php-wasm/runtime/host v0.0.0
	github.com/tetratelabs/wazero v1.7.0
)

replace github.com/php-wasm/runtime/host => ../../runtime/host
