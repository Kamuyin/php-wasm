# Updating Patches

Patches may fail to apply when PHP's upstream source changes. This guide covers
the workflow for refreshing patches after such conflicts.

## Identifying a Failing Patch

When `apply-patches.sh` fails, the output looks like:

```
  Applying: 0003-fork-exec-stubs.patch
error: patch failed: ext/standard/exec.c:44
error: ext/standard/exec.c: patch does not apply
ERROR: Patch failed to apply: 0003-fork-exec-stubs.patch
```

The `--3way` flag ensures git will show you the conflict markers in the file.

## Workflow

### 1. Identify the conflict

```bash
# After a failed apply-patches.sh, inspect the conflicting file
git -C /tmp/php-wasm-build/php-src-php-8.3.30 diff
git -C /tmp/php-wasm-build/php-src-php-8.3.30 status
```

### 2. Check if PHP fixed the issue upstream

Sometimes PHP's own codebase absorbs the fix. Check if the target code still
exists in the same form:

```bash
grep -n "fork\|exec\|popen" /tmp/php-wasm-build/php-src-php-8.3.30/ext/standard/exec.c | head -20
```

If PHP upstream already added a `__wasi__` guard or restructured the code,
you may be able to simply remove the patch from `config.yaml`.

### 3. Refresh the patch against the new source

```bash
# Start from a clean (unpatched) source
cd /tmp/php-wasm-build/php-src-php-8.3.30
git stash  # or re-clone

# Apply all patches up to the failing one
../../../scripts/apply-patches.sh 8.3 . || true

# Manually edit the conflicting file to apply the fix correctly
# (look at the existing patch for intent, apply manually)
vim ext/standard/exec.c

# Stage the fix
git add ext/standard/exec.c

# Generate the new patch
git diff HEAD ext/standard/exec.c > /tmp/new-patch.diff
```

### 4. Update the patch file

The patch in `patches/common/` is a `git format-patch` style file. Update it:

```bash
# Check the old patch
cat patches/common/0003-fork-exec-stubs.patch

# Replace the diff section (keep the header comment block unchanged)
# The header (From:/Subject:/---) must be preserved exactly
```

### 5. Test the updated patch

```bash
# Fresh source tree
./scripts/fetch-source.sh 8.3 /tmp/fresh-build

# Apply all patches including the updated one
./scripts/apply-patches.sh 8.3 /tmp/fresh-build/php-src-php-8.3.30
```

### 6. If the fix is version-specific

If the upstream change only affects PHP 8.3 (not 8.2 or 8.4), move the
updated patch out of `patches/common/` into `versions/8.3/patches/` and
update `config.yaml`:

```yaml
# versions/8.3/config.yaml
common_patches:
  # remove "0003-fork-exec-stubs" from here
  
version_patches:
  - "0001-83-typed-class-constants-fix"
  - "0001-83-fork-exec-stubs-updated"  # version-specific refresh
```

### 7. Check all versions

After updating a common patch, verify it still applies to all tracked versions:

```bash
for ver in 8.2 8.3 8.4; do
    echo "=== Testing PHP ${ver} ==="
    ./scripts/fetch-source.sh ${ver} /tmp/test-${ver}
    ./scripts/apply-patches.sh ${ver} /tmp/test-${ver}/php-src-php-*
done
```

## Common Patch Failure Patterns

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| Context lines changed | Upstream code formatting change | Regenerate patch with updated context |
| Target function moved | Upstream refactor | Find new location, update patch offset |
| Patch already applied | Upstream absorbed the fix | Remove from `common_patches` |
| New guard already exists | PHP added `#ifdef` on different condition | Adapt `__wasi__` guard to coexist |
| Hunk offset mismatch | Lines added/removed near patch target | Run `patch --dry-run --fuzz=3` to see if fuzzy match works |

## Checking VMware WasmLabs and WordPress Playground

The primary patch sources are:
- `vmware-labs/webassembly-language-runtimes` (paths: `languages/php/php-X.Y.Z/patches/`)
- `WordPress/wordpress-playground` (paths: `packages/php-wasm/compile/patches/`)

When a patch fails, check these repos for updated versions of the same fix before
writing a new one from scratch.
