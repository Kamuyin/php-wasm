/* Stub setjmp.h for wasm32-wasi builds targeting wazero.
 *
 * wasi-sdk's setjmp.h requires the WASM Exception Handling proposal, which
 * wazero does not support (its presence inserts a tag section that causes
 * "invalid section order" on load). This stub provides ABI-compatible
 * no-op implementations:
 *   setjmp  — always returns 0; the error recovery branch is never taken
 *   longjmp — emits a WASM unreachable trap; a fatal decode error surfaces
 *             as a wazero module trap (HTTP 500) rather than silent corruption
 *
 * All dep build scripts and configure-php.sh prepend this directory with -I
 * so it is found before the wasi-sysroot target-specific headers.
 */
#ifndef _SETJMP_H
#define _SETJMP_H
typedef struct { int _pad[8]; } jmp_buf[1];
#define setjmp(env) (0)
static __attribute__((noreturn)) inline void longjmp(jmp_buf env, int val) {
    __builtin_trap();
}
#endif /* _SETJMP_H */
