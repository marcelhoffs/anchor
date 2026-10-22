# burrow

Burrow is a self-hosted, multi-user bookmark manager: one Go binary, an embedded SQLite database, one Docker volume.

**Status: milestone 1 (Core).** Accounts with password login and sessions, bookmark CRUD, tags (rename/merge/delete), full-text search, and a Docker image. 2FA, the admin UI, API tokens and the REST API arrive in M2; metadata fetching, import/export and sharing in M3.

## Run it

```sh
mkdir data && sudo chown 65532:65532 data
cp .env.example .env
sed -i "s|^BURROW_SECRET_KEY=.*|BURROW_SECRET_KEY=$(openssl rand -base64 32)|" .env
# edit BURROW_BASE_URL in .env
docker compose up -d
```

Open `BURROW_BASE_URL` and create the first (admin) account on the setup page, or seed it with `BURROW_ADMIN_USER` / `BURROW_ADMIN_PASSWORD`.

## Search syntax

| Query | Meaning |
| --- | --- |
| `kubernetes ingress` | Full-text, all terms (prefix match; title weighted ×3) |
| `"exact phrase"` | Phrase match |
| `#homelab -#archive` | Has tag / lacks tag |
| `site:github.com` | Domain, including subdomains |
| `!fav` `!shared` | Flags |
| `!archived` | Search the archive instead of the main list |
| `after:2026-01-01` `before:2026-06-01` | Saved on/after, before a date (UTC) |

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `BURROW_BASE_URL` | required | Public URL; `https` turns on Secure cookies |
| `BURROW_SECRET_KEY` | required | 32+ random bytes, base64; signs CSRF tokens |
| `BURROW_DATA_DIR` | `/data` | Database location |
| `BURROW_LISTEN` | `:8080` | Bind address |
| `BURROW_TRUSTED_PROXIES` | empty | CIDRs whose `X-Forwarded-For` is honoured |
| `BURROW_LOG_LEVEL` | `info` | `debug` / `info` / `warn` |
| `BURROW_ADMIN_USER`, `BURROW_ADMIN_PASSWORD` | empty | Create the first admin on an empty database |

## Operations

```sh
docker exec burrow /burrow user create alice           # prints a generated password
echo 'pw' | docker exec -i burrow /burrow user create bob --password-stdin
docker exec burrow /burrow user reset-password alice   # also signs out all sessions
docker exec burrow /burrow backup /data/backups/now.db # consistent while running
```

Never copy `burrow.db` on its own while Burrow is running: the `-wal` file holds recent writes. Use `burrow backup`.

## Develop

```sh
go test ./...
BURROW_DATA_DIR=./data BURROW_BASE_URL=http://localhost:8080 \
BURROW_SECRET_KEY=$(openssl rand -base64 32) go run ./cmd/burrow serve
```
