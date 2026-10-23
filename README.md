# anchor

Anchor is a self-hosted, multi-user bookmark manager: one Go binary, an embedded SQLite database, one Docker volume.

**Status: milestones 1 to 3.**

- Bookmarks with tags (created on the Tags page first; names may have spaces), full-text search, favourites and an archive; light/dark themes and a choice of accent colours.
- Page details: an empty title or description, and the site's icon, are fetched from the page for new and imported bookmarks.
- Bundles (saved searches) in the sidebar, and public read-only links for single bookmarks.
- Accounts: two-factor authentication (authenticator app, passkeys, recovery codes), API tokens and a REST API.
- Admin: user management, invites and signup modes, a 2FA policy, rate limits.
- Import from and export to browsers, linkding, Pocket and Pinboard (Netscape HTML) and Anchor JSON.

## Run it

```sh
cp .env.example .env
sed -i "s|^ANCHOR_SECRET_KEY=.*|ANCHOR_SECRET_KEY=$(openssl rand -base64 32)|" .env
# edit ANCHOR_BASE_URL in .env
docker compose up -d
```

Open `ANCHOR_BASE_URL` and create the first (admin) account on the setup page, or seed it with `ANCHOR_ADMIN_USER` / `ANCHOR_ADMIN_PASSWORD`.

Passkeys need `ANCHOR_BASE_URL` to use a domain name over HTTPS (browsers refuse them on IP addresses and plain HTTP). Reached any other way, Anchor runs without passkeys; authenticator apps and recovery codes still work.

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
curl -H "Authorization: Bearer $TOKEN" -d '{"url":"https://go.dev","tags":["go"]}' https://anchor.example.com/api/v1/bookmarks
```

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `ANCHOR_BASE_URL` | required | Public URL; `https` turns on Secure `__Host-` cookies and HSTS, a domain name enables passkeys |
| `ANCHOR_SECRET_KEY` | required | 32+ random bytes, base64; encrypts TOTP secrets, signs CSRF tokens |
| `ANCHOR_DATA_DIR` | `/data` | Database location |
| `ANCHOR_LISTEN` | `:8080` | Bind address |
| `ANCHOR_SIGNUP` | admin's choice (`closed`) | `closed` / `invite` / `open`; when set, overrides the admin page |
| `ANCHOR_REQUIRE_2FA` | admin's choice (`false`) | Require 2FA for everyone; when set, overrides the admin page |
| `ANCHOR_TRUSTED_PROXIES` | empty | CIDRs whose `X-Forwarded-For` is honoured |
| `ANCHOR_FETCH` | `public` | Fetch page details from public addresses only; `all` also allows your own network (loopback, private, Tailscale ranges); `off` disables fetching |
| `ANCHOR_FAVICON_FALLBACK` | `off` | `duckduckgo`, `google` or both (`duckduckgo,google`, asked in that order): when a site serves no icon (or blocks Anchor), ask a public icon service, for the host and then its main domain. The service learns the domain, never the full link. Switching it on fetches icons again for bookmarks that have none |
| `ANCHOR_LOG_LEVEL` | `info` | `debug` / `info` / `warn` |
| `ANCHOR_ADMIN_USER`, `ANCHOR_ADMIN_PASSWORD` | empty | Create the first admin on an empty database |

Keep `ANCHOR_SECRET_KEY` safe and stable: changing it makes existing authenticator-app setups unreadable (users then need a recovery code, or an admin's 2FA reset).

## Operations

```sh
docker exec anchor /anchor user create alice           # prints a generated password
echo 'pw' | docker exec -i anchor /anchor user create bob --password-stdin
docker exec anchor /anchor user reset-password alice   # also signs out all sessions
docker exec anchor /anchor user reset-2fa alice        # lost device: removes all second factors
docker exec anchor /anchor backup /data/backups/now.db # consistent while running
```

Never copy `anchor.db` on its own while Anchor is running: the `-wal` file holds recent writes. Use `anchor backup`.

## Develop

```sh
go test ./...
ANCHOR_DATA_DIR=./data ANCHOR_BASE_URL=http://localhost:8080 \
ANCHOR_SECRET_KEY=$(openssl rand -base64 32) go run ./cmd/anchor serve
```

## License

Anchor is free software under the [GNU Affero General Public License v3.0](LICENSE). You may use, change and share it; if you run a changed version as a network service, you must offer its users the source of your version.

It builds on permissively licensed Go modules (MIT, BSD, Apache-2.0), the [Lucide](https://lucide.dev) icons (ISC) and the [Inter](https://rsms.me/inter/) font (SIL Open Font License, `internal/web/static/fonts/Inter-OFL.txt`). The Docker image carries Anchor's license and those of the modules compiled into it in `/licenses`.
