module github.com/php-wasm/examples/wazero-wordpress

go 1.22

require (
	github.com/php-wasm/examples/cgi v0.0.0
	github.com/tetratelabs/wazero v1.7.0
)

replace github.com/php-wasm/examples/cgi => ../internal/cgi
