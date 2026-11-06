# Deploying Anchor

Anchor is one Go binary with an embedded SQLite database. The Docker image holds nothing but that binary and its licenses; all state lives in one named Docker volume, `anchor-data`, mounted at `/data`.

- [Quick start](#quick-start)
- [The first account](#the-first-account)
- [Configuration](#configuration)
- [HTTPS and reverse proxies](#https-and-reverse-proxies)
- [Single sign-on (OIDC)](#single-sign-on-oidc)
- [Fetching page details](#fetching-page-details)
- [Command line](#command-line)
- [Backups and restores](#backups-and-restores)
- [Upgrades](#upgrades)
- [Running without Docker](#running-without-docker)

## Quick start

You need Docker with the Compose plugin. Every release publishes a ready-made image, so the quickest way needs only two files from this repository; building the image yourself is described [below](#building-the-image-yourself).

### With the ready-made image

The image is `ghcr.io/marcelhoffs/anchor`, for amd64 and arm64. Each release is tagged with its version (`0.7.0`), its minor series (`0.7`, which follows that series' fixes) and `latest`.

```sh
mkdir anchor && cd anchor
curl -fsSLO https://raw.githubusercontent.com/marcelhoffs/anchor/v0.7.0/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/marcelhoffs/anchor/v0.7.0/.env.example
cp .env.example .env
sed -i "s|^ANCHOR_SECRET_KEY=.*|ANCHOR_SECRET_KEY=$(openssl rand -base64 32)|" .env
# Set ANCHOR_BASE_URL in .env to the address people will use.
docker compose pull
docker compose up -d
```

`docker-compose.yml` uses the `latest` tag. To stay on one release series and upgrade only when you choose, pin it:

```sh
sed -i 's|ghcr.io/marcelhoffs/anchor:latest|ghcr.io/marcelhoffs/anchor:0.7|' docker-compose.yml
```

While the repository is private, so are its files and image. Then sign in to the registry first with `docker login ghcr.io`, using your GitHub username and a personal access token with the `read:packages` scope, and get the two files with `gh release download v0.7.0 --pattern '*linux_amd64.tar.gz'` (both are in the archive) or from a clone.

### Building the image yourself

From a copy of this repository, which has the `Dockerfile`:

```sh
cp .env.example .env
sed -i "s|^ANCHOR_SECRET_KEY=.*|ANCHOR_SECRET_KEY=$(openssl rand -base64 32)|" .env
# Set ANCHOR_BASE_URL in .env to the address people will use.
docker compose up -d --build
```

The footer then shows the version as `dev`. To have it name what you built, build with `docker build --build-arg VERSION=$(git describe --tags) -t ghcr.io/marcelhoffs/anchor:latest .` before `docker compose up -d`.

### What you get

Anchor listens on port 8080. The compose file keeps the database in the named volume `anchor-data`, runs the container read-only, and restarts it unless you stop it. Docker creates the volume on the first start, owned by the container's unprivileged user (65532), so there's nothing to prepare; `docker volume inspect anchor-data` shows where it lives on the host. The volume outlives the container: `docker compose down` keeps your data, and only `docker compose down -v` or `docker volume rm anchor-data` deletes it.

`.env.example` starts from the secure choices: two-factor authentication required for everyone, page details fetched from public addresses only, no public icon services, and signup closed (the built-in default). Loosen them in `.env` only when you need to; [Configuration](#configuration) explains each.

The image has a health check (`anchor healthcheck`, which asks the local server's `/healthz`), so `docker ps` shows whether Anchor is up. `docker compose exec anchor /anchor version` prints the version it runs, which the page footer shows too.

## The first account

On an empty database, Anchor's first page is a setup form that creates the first account, which is an admin. To create it without the form, for example in automated setups, set both of these before the first start, and remove them once the account exists:

```sh
ANCHOR_ADMIN_USER=admin
ANCHOR_ADMIN_PASSWORD=at-least-10-characters
```

With `ANCHOR_REQUIRE_2FA=true` (as in `.env.example`), the first admin is asked to set up two-factor authentication right after signing in. After that, admins manage everyone else on the Admin page: create users, send invite links and choose who may sign up.

## Configuration

Anchor reads its settings from `ANCHOR_*` environment variables; the compose file loads them from `.env`. `.env.example` lists every one with a short explanation.

| Variable | Default | Purpose |
| --- | --- | --- |
| `ANCHOR_BASE_URL` | required | The public URL people use, such as `https://anchor.example.com`. `https` turns on Secure `__Host-` cookies and HSTS; a domain name over https enables passkeys. |
| `ANCHOR_SECRET_KEY` | required | 32 or more random bytes, base64-encoded (`openssl rand -base64 32`). Encrypts authenticator-app secrets and signs CSRF tokens. |
| `ANCHOR_SIGNUP` | set on the Admin page (`closed`) | `closed`, `invite` or `open`. When set, it overrides the Admin page. |
| `ANCHOR_REQUIRE_2FA` | set on the Admin page (`false`); `true` in `.env.example` | Require two-factor authentication for everyone. When set, it overrides the Admin page. |
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

## Single sign-on (OIDC)

Anchor can sign people in through an OpenID Connect provider: Authentik, Keycloak, Pocket ID, Entra ID and others. One provider per instance.

| Variable | Default | |
|---|---|---|
| `ANCHOR_OIDC_ISSUER` | empty (off) | The provider's issuer URL, exactly as its discovery document says, trailing slash included. Setting it turns single sign-on on. Use https: over http, Anchor warns at startup (unless the provider is on localhost). |
| `ANCHOR_OIDC_CLIENT_ID` | required with an issuer | The client ID Anchor has at the provider. |
| `ANCHOR_OIDC_CLIENT_SECRET` | empty | The client secret; leave it empty for a public client (Anchor always uses PKCE). |
| `ANCHOR_OIDC_NAME` | `SSO` | Shown on the button: "Sign in with Authentik". |
| `ANCHOR_OIDC_SCOPES` | `openid profile email` | Scopes asked for. Add `groups` if your provider needs that scope for a groups claim. With an admin group, `offline_access` is always added. |
| `ANCHOR_OIDC_ADMIN_GROUP` | empty | Members of this group are admins, everyone else isn't (see [Admins from a group](#admins-from-a-group)). Empty: admins are managed in Anchor. |
| `ANCHOR_OIDC_GROUPS_CLAIM` | `groups` | The claim that lists the groups, in the ID token and the userinfo response. |
| `ANCHOR_OIDC_ONLY` | `false` | `true` hides password sign-in and sign-up, so everyone goes through the provider. |

At the provider, the redirect URI is `{ANCHOR_BASE_URL}/login/oidc/callback`, for example `https://anchor.example.com/login/oidc/callback`. Anchor logs it at startup.

**Who gets an account.** An account at the provider is linked to one Anchor user; Anchor never matches accounts by email address or name. The first sign-in of someone without an account follows the sign-up mode: with *open* sign-up the account is created; with *invite only*, only through an invite link (the invite page offers "Sign up with …"); with *closed*, not at all. On a fresh instance, the setup page offers "Set up with …": that account becomes the first admin. Existing users link their account under Settings → Security.

**Two-factor authentication.** A sign-in through the provider counts as complete, so set up multi-factor authentication there. The "require two-factor authentication" policy then only applies to password sign-ins.

**Admins from a group.** With `ANCHOR_OIDC_ADMIN_GROUP`, the group decides which accounts linked to the provider are admins. Their admin rights hold only in a session signed in through the provider; a password sign-in of a linked account has none, and the admin pages have no *Make admin* or *Remove admin* for them. Local accounts, not linked to the provider, are still made admin (or not) in Anchor; whether they need two-factor authentication follows the "require two-factor authentication" setting as for anyone else. Linking a local admin hands the role to the group, and unlinking an account (also with `anchor user unlink-sso`) takes the group's admin role away. Keep in mind that someone removed from the group keeps admin rights through any local admin account, so check those when someone leaves. The group is read at every sign-in and link through the provider, and is applied even to the last admin. While a session is in use, Anchor asks the provider's userinfo endpoint again at every page load, and for other requests once the last check is more than five minutes old; the admin pages ask on every request. Someone who leaves the group therefore loses admin rights at their next page load, without signing in again, and the admin pages are no longer found.

For these checks Anchor keeps the provider's access and refresh tokens with the session, encrypted with `ANCHOR_SECRET_KEY`, and asks for the `offline_access` scope so the provider hands out a refresh token. When the access token has expired, or the provider refuses it, Anchor renews it with the refresh token. If the provider refuses that too (the user was disabled or removed, or their tokens revoked), or sent no refresh token, Anchor ends the session and the user signs in again. If the provider can't be reached, pages keep working, but the admin pages show an error. Sessions from before Anchor kept tokens end at their next check. If the userinfo response has no groups claim (as with Entra ID), Anchor reads the groups from the ID token that comes with a renewed access token.

**Getting back in.** An account linked to the provider signs in only through it, so disabling someone at the provider keeps them out of Anchor too; their API tokens still work until you disable the account in Anchor as well. A password on a linked account is only for unlinking it. Accounts made through the provider have no password until their owner sets one under Settings → Security. If someone loses their provider account, `anchor user unlink-sso <name>` and `anchor user reset-password <name>` give them a password sign-in again (with `ANCHOR_OIDC_ONLY=true`, switch that off first).

### Authentik

1. **Applications → Providers → Create → OAuth2/OpenID Provider.** Client type *Confidential*; redirect URI `https://anchor.example.com/login/oidc/callback` (strict); any RS256 signing key. Keep the default scope mappings (`openid`, `email`, `profile`): `profile` already includes the user's groups. With an admin group, also select the `offline_access` scope mapping under *Advanced protocol settings*, so Authentik hands out refresh tokens; its *Refresh Token validity* bounds how long a session can go unchecked.
2. **Applications → Applications → Create**, with slug `anchor` and the provider above. Bind a group or policy to choose who may use Anchor.
3. In `.env`:

   ```sh
   ANCHOR_OIDC_ISSUER=https://authentik.example.com/application/o/anchor/
   ANCHOR_OIDC_CLIENT_ID=<client ID>
   ANCHOR_OIDC_CLIENT_SECRET=<client secret>
   ANCHOR_OIDC_NAME=Authentik
   #ANCHOR_OIDC_ADMIN_GROUP=anchor-admins
   ```

### Entra ID

Register an app with the redirect URI as a *Web* platform, create a client secret, and use `https://login.microsoftonline.com/<tenant ID>/v2.0` as the issuer. For an admin group, add the *groups* claim under Token configuration: Entra sends group object IDs, so `ANCHOR_OIDC_ADMIN_GROUP` is the group's object ID.

## Fetching page details

When someone saves a bookmark, Anchor fetches the page for its title, description and icon. By default (`ANCHOR_FETCH=public`) it refuses to connect to loopback, private, link-local and carrier-grade NAT (Tailscale) addresses, so users can't make the server probe your network.

If you bookmark services on your own network, such as `jellyfin.home.example`, set `ANCHOR_FETCH=all`. Every user can then make Anchor fetch from your network, so only do this when you trust your users. `off` turns fetching off altogether.

Some sites serve no icon or block automated requests. `ANCHOR_FAVICON_FALLBACK` asks a public icon service instead. The service learns the bookmarked domain (`example.com`), never the full link, which is why this is off by default. Switching it on fetches icons again, once, for bookmarks that have none.

## Command line

The `anchor` binary also has administration commands. Run them in the running container, from the directory with `docker-compose.yml`:

```sh
docker compose exec anchor /anchor user create alice              # prints a generated password
echo 'a-long-password' | docker compose exec -T anchor /anchor user create bob --password-stdin
docker compose exec anchor /anchor user create carol --admin      # an admin account
docker compose exec anchor /anchor user reset-password alice      # also signs out every session
docker compose exec anchor /anchor user reset-2fa alice           # lost phone: removes all second factors
docker compose exec anchor /anchor user unlink-sso alice          # lost provider account: unlinks single sign-on
docker compose exec anchor /anchor user delete alice              # for good; shared spaces go to an admin
docker compose exec anchor /anchor backup /data/backups/anchor.db # a consistent snapshot, while running
docker compose exec anchor /anchor migrate                        # apply database migrations and exit
```

`anchor help` lists every command.

## Backups and restores

The database is `anchor.db` in the `anchor-data` volume, with `anchor.db-wal` and `anchor.db-shm` beside it. Recent writes live in the `-wal` file, so never copy `anchor.db` on its own while Anchor is running. Use the backup command, which writes a consistent snapshot without stopping anything, then copy the snapshot out of the volume:

```sh
docker compose exec anchor /anchor backup /data/backups/anchor-$(date +%F).db
docker compose cp anchor:/data/backups/anchor-$(date +%F).db .
```

Then keep the copy somewhere else; a backup on the same disk as the database protects you against mistakes, not against losing the disk. A cron job running these two commands and moving the file off the server is enough. Old snapshots stay in `/data/backups` until you remove them.

To restore, stop Anchor, put the snapshot in place of the database, and remove the old `-wal` and `-shm` files so they aren't replayed onto the restored database. The Anchor image has no shell, so a small helper container does the copying, with the snapshot in the current directory:

```sh
docker compose stop
docker run --rm -v anchor-data:/data -v "$PWD":/restore alpine sh -c \
  'cp /restore/anchor-2026-10-23.db /data/anchor.db && rm -f /data/anchor.db-wal /data/anchor.db-shm && chown 65532:65532 /data/anchor.db'
docker compose start
```

Keep the same `ANCHOR_SECRET_KEY`: the backup's authenticator-app secrets are encrypted with it.

## Upgrades

Take a [backup](#backups-and-restores) first: Anchor applies database migrations itself when it starts, before it serves anything, and migrations only go forward; some (such as fetching every site icon again) can't be undone. Read the release notes for anything to do by hand.

With the ready-made image, fetch the new one and recreate the container; the data volume stays:

```sh
docker compose pull
docker compose up -d
```

On a pinned series (`:0.7`) this brings that series' fixes; for a new series, change the tag in `docker-compose.yml` first. Old images stay on disk until `docker image prune` removes them.

Building the image yourself:

```sh
git pull
docker compose up -d --build
```

## Running without Docker

Anchor builds with Go and needs no C compiler: its SQLite driver is pure Go.

```sh
go build -o anchor ./cmd/anchor
ANCHOR_DATA_DIR=./data ANCHOR_BASE_URL=http://localhost:8080 \
ANCHOR_SECRET_KEY=$(openssl rand -base64 32) ./anchor serve
```

For development, `go test ./...` runs the tests, and `go run ./cmd/anchor serve` with the same variables starts a server. A secret key generated on the fly like this changes on every start, which is fine for development only (see [Configuration](#configuration)).
