# Deploying Anchor

Anchor is one Go binary with an embedded SQLite database. The Docker image holds nothing but that binary and its licenses; all state lives in one volume mounted at `/data`.

- [Quick start](#quick-start)
- [The first account](#the-first-account)
- [Configuration](#configuration)
- [HTTPS and reverse proxies](#https-and-reverse-proxies)
- [Fetching page details](#fetching-page-details)
- [Command line](#command-line)
- [Backups and restores](#backups-and-restores)
- [Upgrades](#upgrades)
- [Running without Docker](#running-without-docker)

## Quick start

You need Docker with the Compose plugin, and a copy of this repository (for `docker-compose.yml`, `.env.example` and the `Dockerfile`).

```sh
cp .env.example .env
sed -i "s|^ANCHOR_SECRET_KEY=.*|ANCHOR_SECRET_KEY=$(openssl rand -base64 32)|" .env
# Set ANCHOR_BASE_URL in .env to the address people will use.
mkdir data && sudo chown 65532:65532 data
docker compose up -d --build
```

Anchor listens on port 8080. The compose file mounts `./data` as the data volume, runs the container read-only, and restarts it unless you stop it. The container runs as the unprivileged user 65532, so a bind-mounted data directory must be writable by that user (the `chown` above); a named Docker volume needs nothing.

The image has a health check (`anchor healthcheck`, which asks the local server's `/healthz`), so `docker ps` shows whether Anchor is up.

## The first account

On an empty database, Anchor's first page is a setup form that creates the first account, which is an admin. To create it without the form, for example in automated setups, set both of these before the first start, and remove them once the account exists:

```sh
ANCHOR_ADMIN_USER=admin
ANCHOR_ADMIN_PASSWORD=at-least-10-characters
```

After that, admins manage everyone else on the Admin page: create users, send invite links, choose who may sign up, and require two-factor authentication.

## Configuration

Anchor reads its settings from `ANCHOR_*` environment variables; the compose file loads them from `.env`. `.env.example` lists every one with a short explanation.

| Variable | Default | Purpose |
| --- | --- | --- |
| `ANCHOR_BASE_URL` | required | The public URL people use, such as `https://anchor.example.com`. `https` turns on Secure `__Host-` cookies and HSTS; a domain name over https enables passkeys. |
| `ANCHOR_SECRET_KEY` | required | 32 or more random bytes, base64-encoded (`openssl rand -base64 32`). Encrypts authenticator-app secrets and signs CSRF tokens. |
| `ANCHOR_SIGNUP` | set on the Admin page (`closed`) | `closed`, `invite` or `open`. When set, it overrides the Admin page. |
| `ANCHOR_REQUIRE_2FA` | set on the Admin page (`false`) | Require two-factor authentication for everyone. When set, it overrides the Admin page. |
| `ANCHOR_TRUSTED_PROXIES` | empty | CIDRs of reverse proxies whose `X-Forwarded-For` header is trusted. |
| `ANCHOR_FETCH` | `public` | Where page details may be fetched from: `public` (internet addresses only), `all` (also your own network) or `off`. |
| `ANCHOR_FAVICON_FALLBACK` | `off` | `duckduckgo`, `google`, or both (`duckduckgo,google`, asked in that order) for sites that serve no icon. |
| `ANCHOR_LOG_LEVEL` | `info` | `debug`, `info` or `warn`. |
| `ANCHOR_ADMIN_USER`, `ANCHOR_ADMIN_PASSWORD` | empty | Create the first admin on an empty database. |
| `ANCHOR_DATA_DIR` | `/data` | Where the database lives. |
| `ANCHOR_LISTEN` | `:8080` | The address and port to listen on. |

Keep `ANCHOR_SECRET_KEY` safe and never change it on a running instance: authenticator-app setups encrypted with the old key can't be read with a new one. Users would then need a recovery code, or an admin to reset their two-factor authentication.

## HTTPS and reverse proxies

Run Anchor behind a reverse proxy that terminates HTTPS (Caddy, Traefik, nginx, a Cloudflare or Pangolin tunnel), and set `ANCHOR_BASE_URL` to the `https://` address.

Over HTTPS, Anchor's cookies get the `__Host-` prefix and the Secure flag, and responses carry HSTS. Over plain HTTP everything works except passkeys: browsers allow them only on a domain name over HTTPS, so Anchor then hides them. Authenticator apps and recovery codes work either way.

Anchor limits login, signup and second-factor attempts per client address. Behind a proxy, every request seems to come from the proxy, so tell Anchor which addresses are proxies:

```sh
# The Docker network's gateway, if the proxy reaches Anchor through it.
ANCHOR_TRUSTED_PROXIES=172.19.0.1/32
```

Anchor then takes the client address from `X-Forwarded-For`, but only for requests that come from those addresses. Keep the range as narrow as you can: anything in it can claim to be any client.

## Fetching page details

When someone saves a bookmark, Anchor fetches the page for its title, description and icon. By default (`ANCHOR_FETCH=public`) it refuses to connect to loopback, private, link-local and carrier-grade NAT (Tailscale) addresses, so users can't make the server probe your network.

If you bookmark services on your own network, such as `jellyfin.home.example`, set `ANCHOR_FETCH=all`. Every user can then make Anchor fetch from your network, so only do this when you trust your users. `off` turns fetching off altogether.

Some sites serve no icon or block automated requests. `ANCHOR_FAVICON_FALLBACK` asks a public icon service instead. The service learns the bookmarked domain (`example.com`), never the full link, which is why this is off by default. Switching it on fetches icons again, once, for bookmarks that have none.

## Command line

The `anchor` binary also has administration commands. Run them inside the container:

```sh
docker exec anchor /anchor user create alice                   # prints a generated password
echo 'a-long-password' | docker exec -i anchor /anchor user create bob --password-stdin
docker exec anchor /anchor user create carol --admin           # an admin account
docker exec anchor /anchor user reset-password alice           # also signs out every session
docker exec anchor /anchor user reset-2fa alice                # lost phone: removes all second factors
docker exec anchor /anchor backup /data/backups/anchor.db      # a consistent snapshot, while running
docker exec anchor /anchor migrate                             # apply database migrations and exit
```

`anchor help` lists every command.

## Backups and restores

The database is `anchor.db` in the data directory, with `anchor.db-wal` and `anchor.db-shm` beside it. Recent writes live in the `-wal` file, so never copy `anchor.db` on its own while Anchor is running. Use the backup command, which writes a consistent snapshot without stopping anything:

```sh
docker exec anchor /anchor backup /data/backups/anchor-$(date +%F).db
```

Then copy the snapshot somewhere else; a backup on the same disk as the database protects you against mistakes, not against losing the disk. A cron job running the command above and copying the file off the server is enough.

To restore, stop Anchor, replace the database with the snapshot, and remove the old `-wal` and `-shm` files so they aren't replayed onto the restored database:

```sh
docker compose stop
cp data/backups/anchor-2026-10-23.db data/anchor.db
rm -f data/anchor.db-wal data/anchor.db-shm
docker compose start
```

Keep the same `ANCHOR_SECRET_KEY`: the backup's authenticator-app secrets are encrypted with it.

## Upgrades

```sh
git pull
docker compose up -d --build
```

Anchor applies database migrations itself when it starts, before it serves anything. Take a backup first: migrations only go forward, and some (such as fetching every site icon again) can't be undone.

## Running without Docker

Anchor builds with Go and needs no C compiler: its SQLite driver is pure Go.

```sh
go build -o anchor ./cmd/anchor
ANCHOR_DATA_DIR=./data ANCHOR_BASE_URL=http://localhost:8080 \
ANCHOR_SECRET_KEY=$(openssl rand -base64 32) ./anchor serve
```

For development, `go test ./...` runs the tests, and `go run ./cmd/anchor serve` with the same variables starts a server. A secret key generated on the fly like this changes on every start, which is fine for development only (see [Configuration](#configuration)).
