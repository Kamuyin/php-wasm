# Publishing WordPress artifacts to GitHub Packages (ghcr.io)

The Gimpel `wordpress-honeypot` module fetches two OCI artifacts at deploy time,
by version, from GitHub Packages:

| Artifact | Reference | Built by |
|----------|-----------|----------|
| PHP wasm (wordpress profile) | `ghcr.io/<owner>/php-wasm:<php_version>-wordpress` | `scripts/package.sh` |
| WordPress asset bundle | `ghcr.io/<owner>/wordpress-assets:<wp_version>` | `scripts/assemble-wordpress-assets.sh` |

`<owner>` is your GitHub username/org, lowercased (e.g. `kamyuin`).

## CI (recommended)

Run the **Publish WordPress artifacts** workflow
(`.github/workflows/publish-wordpress.yml`) from the Actions tab, or:

```bash
gh workflow run publish-wordpress.yml \
  -f php_version=8.3 -f wordpress_version=6.7
```

It builds the wasm, assembles the asset bundle, and pushes both. It authenticates
with the built-in `GITHUB_TOKEN` (`packages: write`) — no secrets to configure.

## After the first publish: make the packages public

New GHCR packages are **private** by default. The Gimpel agent pulls anonymously,
so make each package public once:

GitHub → your profile → **Packages** → `php-wasm` → **Package settings** →
**Change visibility → Public**. Repeat for `wordpress-assets`.

(Alternatively keep them private and give the agent a pull token: run
`oras login ghcr.io -u <user> -p <PAT>` on each agent host, where the PAT has
`read:packages`.)

## Local publishing (no CI)

Needs `oras` and a GitHub Personal Access Token with `write:packages`:

```bash
export GITHUB_REPOSITORY_OWNER=<your-user>
echo "$GHCR_PAT" | oras login ghcr.io -u "$GITHUB_REPOSITORY_OWNER" --password-stdin

# PHP wasm (build first: make docker-build VERSION=8.3 PROFILE=wordpress)
scripts/package.sh out/php-8.3.30-wordpress.wasm
oras tag ghcr.io/<owner>/php-wasm:8.3.30-wordpress ghcr.io/<owner>/php-wasm:8.3-wordpress

# WordPress asset bundle
WP_VERSION=6.7 scripts/assemble-wordpress-assets.sh --push
```

## What's in the asset bundle

`assemble-wordpress-assets.sh` produces a docroot tar (untars to `index.php` at
the mount root) containing WordPress core plus, baked in:

- **SQLite drop-in** (`wp-content/db.php`) from `sqlite-database-integration` — no MySQL.
- **`mu-plugins/wasi-compat.php`** — short-circuits outbound HTTP (WASI has no sockets).
- **`mu-plugins/gimpel-writable-paths.php`** — redirects uploads to `/data/uploads`.
- **`wp-config.php`** — `DB_DIR=/data/database/`, cron/updates/external-HTTP disabled.

Gimpel mounts this **read-only** at `/app`; all writable state goes to the
module's `/data` (persistent) and `/tmp` (ephemeral) mounts. Keep these paths in
sync with `modules/wordpress-wasm/gimpel.module.toml` in the Gimpel repo.
