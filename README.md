# burrow

Burrow is a self-hosted, multi-user bookmark manager: one Go binary, an embedded SQLite database, one Docker volume.

**Status: milestones 1 to 3.**

- Bookmarks with tags (created on the Tags page first; names may have spaces), full-text search, favourites and an archive; light/dark themes and a choice of accent colours.
- Page details: an empty title or description, and the site's icon, are fetched from the page for new and imported bookmarks.
- Bundles (saved searches) in the sidebar, and public read-only links for single bookmarks.
- Accounts: two-factor authentication (authenticator app, passkeys, recovery codes), API tokens and a REST API.
- Admin: user management, invites and signup modes, a 2FA policy, rate limits.
- Import from and export to browsers, linkding, Pocket and Pinboard (Netscape HTML) and Burrow JSON.

## Run it

```sh
cp .env.example .env
sed -i "s|^BURROW_SECRET_KEY=.*|BURROW_SECRET_KEY=$(openssl rand -base64 32)|" .env
# edit BURROW_BASE_URL in .env
docker compose up -d
```

Open `BURROW_BASE_URL` and create the first (admin) account on the setup page, or seed it with `BURROW_ADMIN_USER` / `BURROW_ADMIN_PASSWORD`.

Passkeys need `BURROW_BASE_URL` to use a domain name over HTTPS (browsers refuse them on IP addresses and plain HTTP). Reached any other way, Burrow runs without passkeys; authenticator apps and recovery codes still work.

## Search syntax

| Query | Meaning |
| --- | --- |
| `kubernetes ingress` | Full-text, all terms (prefix match; title weighted ×3) |
| `"exact phrase"` | Phrase match |
| `#homelab -#archive` | Has tag / lacks tag |
| `#"home lab"` | A tag with spaces (also `-#"home lab"`) |
| `site:github.com` | Domain, including subdomains |
| `!fav` `!shared` | Flags |
| `!archived` | Search the archive instead of the main list |
| `after:2026-01-01` `before:2026-06-01` | Saved on/after, before a date (UTC) |

## REST API

JSON under `/api/v1`, authenticated with a personal token (Settings → API tokens) sent as `Authorization: Bearer <token>`. Tokens are read-only or read & write, and limited to 600 requests a minute. Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem JSON.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/bookmarks?q=&tag=&archived=&favourite=&limit=&cursor=` | List / search; follow `next` for more |
| POST | `/api/v1/bookmarks` | Create (200 with the existing bookmark if the URL is saved) |
| GET | `/api/v1/bookmarks/check?url=` | Is this URL saved? |
| GET / PATCH / DELETE | `/api/v1/bookmarks/{id}` | Read / update (send only the fields to change) / delete |
| POST | `/api/v1/bookmarks/{id}/archive` | Archive; `{"archived": false}` unarchives |
| POST | `/api/v1/bookmarks/{id}/share` | Make a public link (`share_url`); `{"shared": false}` removes it |
| GET | `/api/v1/bundles` | Saved searches; list one with `?q=` its `query` |
| GET | `/api/v1/tags` | Tags with counts |
| POST | `/api/v1/tags` | Create a tag (`{"name"}`); bookmarks can only use tags that exist |
| PATCH / DELETE | `/api/v1/tags/{id}` | Rename (`{"name"}`), merge (`{"merge_into": id}`), delete |
| POST | `/api/v1/import` | Upload a bookmark file (body or multipart `file`); returns a job |
| GET | `/api/v1/jobs/{id}` | Import progress and result |
| GET | `/api/v1/export?format=html\|json` | Export everything |
| GET | `/api/v1/user` | The token's user |

```sh
curl -H "Authorization: Bearer $TOKEN" -d '{"url":"https://go.dev","tags":["go"]}' https://burrow.example.com/api/v1/bookmarks
```

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `BURROW_BASE_URL` | required | Public URL; `https` turns on Secure `__Host-` cookies and HSTS, a domain name enables passkeys |
| `BURROW_SECRET_KEY` | required | 32+ random bytes, base64; encrypts TOTP secrets, signs CSRF tokens |
| `BURROW_DATA_DIR` | `/data` | Database location |
| `BURROW_LISTEN` | `:8080` | Bind address |
| `BURROW_SIGNUP` | admin's choice (`closed`) | `closed` / `invite` / `open`; when set, overrides the admin page |
| `BURROW_REQUIRE_2FA` | admin's choice (`false`) | Require 2FA for everyone; when set, overrides the admin page |
| `BURROW_TRUSTED_PROXIES` | empty | CIDRs whose `X-Forwarded-For` is honoured |
| `BURROW_FETCH` | `public` | Fetch page details from public addresses only; `all` also allows your own network (loopback, private, Tailscale ranges); `off` disables fetching |
| `BURROW_LOG_LEVEL` | `info` | `debug` / `info` / `warn` |
| `BURROW_ADMIN_USER`, `BURROW_ADMIN_PASSWORD` | empty | Create the first admin on an empty database |

Keep `BURROW_SECRET_KEY` safe and stable: changing it makes existing authenticator-app setups unreadable (users then need a recovery code, or an admin's 2FA reset).

## Operations

```sh
docker exec burrow /burrow user create alice           # prints a generated password
echo 'pw' | docker exec -i burrow /burrow user create bob --password-stdin
docker exec burrow /burrow user reset-password alice   # also signs out all sessions
docker exec burrow /burrow user reset-2fa alice        # lost device: removes all second factors
docker exec burrow /burrow backup /data/backups/now.db # consistent while running
```

Never copy `burrow.db` on its own while Burrow is running: the `-wal` file holds recent writes. Use `burrow backup`.

## Develop

```sh
go test ./...
BURROW_DATA_DIR=./data BURROW_BASE_URL=http://localhost:8080 \
BURROW_SECRET_KEY=$(openssl rand -base64 32) go run ./cmd/burrow serve
```
