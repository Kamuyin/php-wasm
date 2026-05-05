# Architecture

## Overview

`php-wasm` is a **build system**, not a PHP fork. It fetches official PHP source
tarballs, applies targeted WASI-compatibility patches, and produces
`wasm32-wasi` binaries using `wasi-sdk` (Clang + `wasm-ld`).

```
┌─────────────────────────────────────────────────────────────┐
│                      php-wasm repo                          │
│                                                             │
│  versions/X.Y/          profiles/          patches/         │
│  ├─ source.yaml         ├─ minimal.yaml    └─ common/       │
│  ├─ config.yaml         ├─ default.yaml        0001-*.patch │
│  └─ patches/            ├─ full.yaml            ...         │
│      version-specific   └─ honeypot.yaml                    │
│                                                             │
│  scripts/                                                   │
│  ├─ fetch-source.sh     ← downloads + verifies tarball      │
│  ├─ apply-patches.sh    ← common then version patches       │
│  ├─ configure-php.sh    ← builds ./configure invocation     │
│  ├─ build.sh            ← orchestrates full pipeline        │
│  ├─ package.sh          ← OCI push + cosign sign            │
│  └─ check-upstream.sh   ← php.net API check, issue bot      │
│                                                             │
│  docker/Dockerfile.builder  ← reproducible build env       │
└─────────────────────────────────────────────────────────────┘
          │
          ▼
   out/php-{ver}-{profile}.wasm
   out/php-{ver}-{profile}.wasm.sha256
          │
          ├── GitHub Release assets
          └── OCI Artifact (ghcr.io)
```

## Build Pipeline

```
fetch-source.sh          apply-patches.sh        configure-php.sh
  ┌──────────┐           ┌──────────────┐         ┌──────────────┐
  │ Download │──sha256──▶│ git apply    │──config─▶│ ./configure  │
  │ tarball  │           │ --3way       │          │ wasm32-wasi  │
  └──────────┘           └──────────────┘          └──────────────┘
                                                          │
                                                   make -j$(nproc)
                                                          │
                                                   llvm-strip + wasm-opt
                                                          │
                                                   php-{ver}-{profile}.wasm
```

## Toolchain

| Component | Version | Purpose |
|-----------|---------|---------|
| wasi-sdk | ≥ 24 | Clang + wasm-ld targeting `wasm32-wasi` |
| wasm-opt | bundled in binaryen | `-Oz` binary size optimization |
| wasm-strip | from wabt | Strip DWARF debug sections |
| cosign | latest | Keyless signing of OCI artifacts |
| oras | ≥ 1.1 | Push WASM as OCI artifact to GHCR |

## WASI Emulation Layers

PHP requires several POSIX APIs that wasm32-wasi Preview1 does not natively
provide. These are handled in two ways:

**1. wasi-libc emulation libraries** (linked via `LDFLAGS`):

| Library | Emulates |
|---------|---------|
| `libwasi-emulated-signal.a` | `signal()`, `raise()`, `sigaction()` |
| `libwasi-emulated-getpid.a` | `getpid()`, `getppid()` |
| `libwasi-emulated-process-clocks.a` | `clock_gettime()` for process clocks |
| `libwasi-emulated-mman.a` | `mmap()`, `munmap()` via `memory.grow` |

**2. Source patches** (in `patches/common/`):

| Patch | What it fixes |
|-------|--------------|
| `0001-wasi-mmap-alignment` | WASM page-size alignment for mmap calls |
| `0002-disable-fiber-asm` | Replaces ucontext fibers with ENOSYS stub |
| `0003-fork-exec-stubs` | ENOSYS stubs for exec/popen/proc_open |
| `0004-setjmp-wasi` | Compiler barrier for setjmp/longjmp under LTO |
| `0005-wasi-stdin-eof` | EAGAIN retry for non-blocking WASI stdin |
| `0006-disable-opcache` | JIT/opcache disabled (no RWX pages in WASM) |
| `0007-safe-fd-macros` | FD_SETSIZE guard for WASI fd numbers |
| `0099-version-banner` | Appends `-wasi` to PHP_VERSION |

## WASM Module Interface

The output binary implements `wasi_snapshot_preview1` imports only.
There are no custom imports beyond the standard WASI surface.

**Exports** (from `_start` / CGI SAPI):
- `_start` — WASI command entry point; PHP reads env vars and stdin

**Filesystem conventions** (pre-opened at instantiation time):
- `/srv/app` — document root (PHP scripts)
- `/tmp` — temporary files
- `/etc/php` — php.ini location (optional)

See `docs/CONSUMER_API.md` for the full consumer contract.

## Multi-Version Strategy

Each PHP version lives in `versions/X.Y/` with its own `config.yaml` and
optional version-specific patches in `versions/X.Y/patches/`. Common patches
live in `patches/common/` and are applied to all versions listed in
`common_patches` in `config.yaml`.

When a patch fails to apply cleanly (e.g. due to PHP upstream changes), see
`docs/UPDATING_PATCHES.md`.
