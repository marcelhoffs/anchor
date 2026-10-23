# How Anchor works

This page explains what Anchor does behind its pages: how it stores bookmarks, searches them, fetches page details, imports and exports, and keeps accounts safe. For running it, see [Deployment](deployment.md); for the API, see [REST API](api.md).

- [Overview](#overview)
- [Bookmarks](#bookmarks)
- [Tags](#tags)
- [Search](#search)
- [Bundles and sharing](#bundles-and-sharing)
- [Page details and icons](#page-details-and-icons)
- [Import and export](#import-and-export)
- [Accounts and sign-in](#accounts-and-sign-in)
- [Security measures](#security-measures)

## Overview

Anchor is a single Go program. It serves HTML pages rendered on the server (with a little JavaScript for passkeys and tag pickers), a JSON API under `/api/v1`, and runs two background workers: one for imports and one for fetching page details.

Everything is stored in one SQLite database, `anchor.db`, in write-ahead-log mode. All writes go through one connection, so they never conflict; reads use a separate pool and don't wait for writes. Background work keeps its queue in the database too, so a restart picks up where it left off.

Every user's data is private. The data layer takes the owning user as a required argument on every query that touches user data, so one user can't reach another's bookmarks, tags or tokens. The only things shared between users are site icons, which belong to a domain, not a person.

## Bookmarks

A bookmark has a URL, a title, a description, your notes, tags, and two flags: favourite and archived.

**One bookmark per URL.** Before saving, Anchor normalises the URL: it adds `https://` if no scheme was given, lower-cases the scheme and host, drops the default port (`:443` for https, `:80` for http), and adds a `/` path if there is none. Only `http` and `https` links are accepted. Two spellings of the same address therefore become the same bookmark: saving a URL you already have opens your existing bookmark instead of making a second one.

**Favourites and archive.** Favourites are a filter, not a separate list: they stay in the main list and also appear under Favourites. Archived bookmarks leave the main list and searches, and are found under Archive or with `!archived`.

**Saving from anywhere.** Besides the add form there is a bookmarklet (Settings → Profile): a browser bookmark that opens Anchor's add form with the current page's address and title filled in. The [REST API](api.md) can save bookmarks too.

## Tags

Tags are created on the Tags page before bookmarks can use them, which keeps typos from turning into new tags. Names may contain spaces but not commas or double quotes.

Renaming a tag to a name you already have merges the two: every bookmark gets the surviving tag. Deleting a tag removes it from its bookmarks and leaves the bookmarks themselves alone.

Imports are the exception to "create first": an import creates the tags it brings with it.

## Search

The search box understands a small query language. Terms combine: every condition must hold.

| Query | Finds |
| --- | --- |
| `kubernetes ingress` | Bookmarks containing all these words, as whole words or word beginnings |
| `"exact phrase"` | This exact phrase |
| `#homelab` | Bookmarks with this tag |
| `-#homelab` | Bookmarks without this tag |
| `#"home lab"` | A tag with spaces (also `-#"home lab"`) |
| `site:github.com` | Bookmarks on this site, including its subdomains |
| `!fav` | Favourites |
| `!shared` | Bookmarks with a public link |
| `!archived` | Search the archive instead of the main list |
| `after:2026-01-01` | Saved on or after this date (UTC) |
| `before:2026-06-01` | Saved before this date (UTC) |

Words are searched in titles, descriptions, notes, URLs and tags, ignoring case and accents (`cafe` finds `café`). Matches are ranked by relevance, with a match in the title counting three times as much as one elsewhere. Without search words, bookmarks are listed newest first, or by title if you chose that under Settings.

Search uses SQLite's full-text index (FTS5), which is kept up to date as bookmarks change.

## Bundles and sharing

**Bundles** are saved searches. Run a search, choose "Save search", and the bundle appears in the sidebar. It stores the query, not a list of bookmarks, so new bookmarks that match show up in it automatically.

**Public links** make one bookmark readable by anyone with the link, at `/s/<token>`. The page shows the bookmark's title, site and description; never your notes, tags or other bookmarks. The token is random and unguessable. Turning sharing off breaks the link for good; turning it on again makes a new one. A disabled account's links stop working too.

## Page details and icons

When a bookmark is saved or imported, it joins a queue for page details. A background fetcher takes the newest bookmarks first, four at a time, so a large import doesn't hold up the link you just saved.

**What is fetched.** The fetcher downloads the page (at most 10 seconds and the first 1 MiB) and reads its `<head>`: the title, the description (or the Open Graph title and description when the plain ones are missing) and the icon links. Fetched details only fill fields that are still empty, so they never overwrite what you typed. "Fetch again" on the edit page queues a bookmark again.

**Redirects.** If the page redirects to another host, that's usually a sign-in page or a cookie wall, whose title and icon would be wrong. Anchor then ignores that page. The exception is a redirect to the site's own main host (`online.example.com` to `www.example.com`): its icons are the site's, so Anchor uses them.

**Icons.** Anchor tries the icons the page links to, then `/favicon.ico`. It accepts PNG, GIF, JPEG, WebP and ICO files up to 100 KiB, checking the actual file contents rather than trusting the server's label. SVG icons are refused, because SVG can contain scripts. Icons with no visible pixel (some sites serve a fully transparent `favicon.ico`) are skipped as well.

Icons are stored once per host and shared by every bookmark, and every user, on that host. A stored icon is reused for 30 days before it is fetched again. If a site has no usable icon, the bookmark shows the first letter of the site instead.

**Icon services.** When enabled with `ANCHOR_FAVICON_FALLBACK`, Anchor asks DuckDuckGo or Google for an icon that a site doesn't provide itself, first for the host and then for its main domain. A service tends to answer for a product host (`keep.google.com`) with the parent company's logo, so such hosts get the letter instead. A host the service doesn't know at all is usually a customer's space on a platform (`yourcompany.sharepoint.com`), where the platform's icon fits.

**Network safety.** Users choose the URLs Anchor fetches, so by default Anchor refuses to connect to private, loopback and other non-public addresses. The address is checked on every connection, including after redirects and DNS lookups, so a public name that points to a private address is refused as well. See [Fetching page details](deployment.md#fetching-page-details) to allow your own network.

## Import and export

**Import** (Settings → Import & export, or the API) reads:

- Netscape bookmark HTML, as exported by every browser, linkding, Pocket and Pinboard. Folders become tags (except browsers' own root folders such as "Bookmarks bar", which would tag almost everything), and the files' own tags, descriptions and dates are kept.
- Anchor's JSON export.

Files up to 32 MiB are accepted. The import runs in the background in small batches, so it doesn't slow down other users, and the page shows its progress. A URL you already have isn't duplicated: the existing bookmark gains the imported tags, empty fields are filled in, and favourite or archived can be turned on but never off. Links that aren't http or https are skipped and counted. Imported bookmarks then join the page-details queue.

**Export** writes all your bookmarks as Netscape HTML, which any browser can import, or as Anchor JSON, which keeps everything Anchor knows, including notes and flags.

## Accounts and sign-in

**Signup** is `closed` (admins create accounts), `invite` (people sign up with a single-use link from an admin, valid for 7 days) or `open`. Admins choose on the Admin page, unless `ANCHOR_SIGNUP` fixes it.

**Passwords** are at least 10 characters and are hashed with Argon2id (19 MiB of memory, 2 passes). Anchor limits how many hashes it computes at once, so a burst of logins can't exhaust the server's memory.

**Two-factor authentication** adds a second step after the password, with any of:

- an authenticator app (TOTP). Its secret is stored encrypted, and each code works only once, even if two requests race;
- a passkey (a security key, phone or password manager). Passkeys need Anchor on a domain name over HTTPS;
- one of 10 single-use recovery codes, for when the other factors are lost. They are stored as hashes.

Admins can require two-factor authentication for everyone. Users without it are then held on the Security page until they set it up.

**Sessions** last 30 days from the last visit and are listed under Settings → Security, where each can be signed out. Changing or resetting a password signs out every session. Sensitive actions (changing second factors, creating API tokens, admin actions) ask you to confirm your identity again if you haven't in the last 10 minutes.

**API tokens** are personal, read-only or read & write, and can expire. See [REST API](api.md).

**Admins** can create, disable and re-enable users, make them admins or not, reset a password (which shows a new one once) and reset two-factor authentication for someone who lost their device. The same resets are available on the [command line](deployment.md#command-line), for when no admin can sign in.

## Security measures

- **Secrets at rest.** Session cookies, API tokens, invite links and recovery codes are random values of which Anchor stores only a SHA-256 hash: a copy of the database doesn't let anyone sign in. Authenticator-app secrets are encrypted with AES-256-GCM, under a key derived from `ANCHOR_SECRET_KEY` for that purpose alone.
- **Cookies.** Over HTTPS, cookies use the `__Host-` prefix, Secure and HttpOnly, so they can't be read by scripts or set by other sites or subdomains.
- **Cross-site requests.** Every form and state-changing request carries a CSRF token that is signed with a key derived from `ANCHOR_SECRET_KEY` and must match a cookie.
- **Rate limits.** Logins are limited to 10 attempts per account and 100 per client address every 15 minutes, second-factor codes to 10 per account every 15 minutes, signups to 10 per address per hour, and each API token to 600 requests a minute.
- **Response headers.** A strict Content Security Policy allows only Anchor's own scripts, styles and images, with no inline scripts; pages can't be framed; HSTS is sent over HTTPS; and referrers aren't sent to other sites.
- **Fetching.** Page details are fetched only from public addresses unless you allow more, and SVG icons are never accepted (see [Page details and icons](#page-details-and-icons)).
- **Container.** The Docker image is distroless: no shell or package manager, and Anchor runs as an unprivileged user. The provided compose file also makes the container's file system read-only, so only the data volume is writable.
