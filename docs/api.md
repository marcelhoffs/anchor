# REST API

Anchor's API speaks JSON under `/api/v1`. Everything you can do with your own bookmarks in the web interface, you can do through it.

## Authentication

Create a personal token under Settings → API tokens and send it in the `Authorization` header:

```sh
curl -H "Authorization: Bearer $TOKEN" https://anchor.example.com/api/v1/user
```

A token is read-only or read & write, may have an expiry date, and acts as the user who created it: it sees only the spaces that user is in. Anchor shows a token once, when it is created, and stores only its hash; a lost token can't be shown again, only revoked and replaced. Disabling a user also stops their tokens.

Each token may make 600 requests a minute; beyond that Anchor answers `429 Too Many Requests` with a `Retry-After` header.

## Spaces

Every endpoint works on one [space](how-it-works.md#spaces): your personal space, or the shared space named by `?space=<id>`. `GET /api/v1/spaces` lists the ids. Ids of bookmarks and tags are looked up within that space, so a bookmark in a shared space needs its `?space=` too.

A space you aren't in answers `404`. Changes need both a read & write token and a role that allows them: a read & write token of a read-only member gets `403`.

```sh
curl -H "Authorization: Bearer $TOKEN" https://anchor.example.com/api/v1/spaces
curl -H "Authorization: Bearer $TOKEN" "https://anchor.example.com/api/v1/bookmarks?space=7"
```

## Errors

Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details, with the `application/problem+json` content type. Their messages are always in English, whatever language the web interface uses:

```json
{"type": "about:blank", "title": "Not Found", "status": 404, "detail": "Not found."}
```

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/bookmarks?q=&tag=&archived=&favourite=&limit=&cursor=` | List or search bookmarks; follow `next` for the next page |
| POST | `/api/v1/bookmarks` | Create a bookmark (answers 200 with the existing bookmark if the URL is already saved) |
| GET | `/api/v1/bookmarks/check?url=` | Is this URL saved? |
| GET | `/api/v1/bookmarks/{id}` | Read a bookmark |
| PATCH | `/api/v1/bookmarks/{id}` | Update a bookmark; send only the fields to change |
| DELETE | `/api/v1/bookmarks/{id}` | Delete a bookmark |
| POST | `/api/v1/bookmarks/{id}/archive` | Archive; `{"archived": false}` unarchives |
| POST | `/api/v1/bookmarks/{id}/share` | Make a public link (`share_url`); `{"shared": false}` removes it |
| GET | `/api/v1/bundles` | Saved searches; list a bundle's bookmarks with `?q=` set to its `query` |
| GET | `/api/v1/tags` | Tags with their bookmark counts |
| POST | `/api/v1/tags` | Create a tag (`{"name": "home lab"}`); bookmarks can only use tags that exist |
| PATCH | `/api/v1/tags/{id}` | Rename (`{"name": …}`) or merge into another tag (`{"merge_into": id}`) |
| DELETE | `/api/v1/tags/{id}` | Delete a tag; its bookmarks stay |
| POST | `/api/v1/import` | Upload a bookmark file (as the body, or as a multipart `file` field); returns a job |
| GET | `/api/v1/jobs/{id}` | An import's progress and result |
| GET | `/api/v1/export?format=html\|json` | Export all bookmarks in the space |
| GET | `/api/v1/user` | The token's user |
| GET | `/api/v1/spaces` | Your spaces: the personal one first, then the shared ones, with your `role` in each |

The `q` parameter takes the same [search syntax](how-it-works.md#search) as the search box.

## Examples

Save a bookmark with a tag (the tag must exist):

```sh
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url": "https://go.dev", "tags": ["go"]}' \
  https://anchor.example.com/api/v1/bookmarks
```

Find favourites on GitHub:

```sh
curl -G -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'q=site:github.com !fav' \
  https://anchor.example.com/api/v1/bookmarks
```

Import a browser export and follow the job:

```sh
curl -H "Authorization: Bearer $TOKEN" -F file=@bookmarks.html https://anchor.example.com/api/v1/import
curl -H "Authorization: Bearer $TOKEN" https://anchor.example.com/api/v1/jobs/42
```
