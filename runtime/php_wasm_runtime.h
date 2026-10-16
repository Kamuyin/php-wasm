/* Private PHP runtime integration. No PHP functions, modules or INI entries. */
#ifndef PHP_WASM_RUNTIME_H
#define PHP_WASM_RUNTIME_H
static void php_wasm_runtime_startup(void);
static void php_wasm_runtime_activate(void);
static void php_wasm_runtime_deactivate(void);
#endif
