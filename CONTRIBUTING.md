# Contributing to Anchor

Bug reports, translations and pull requests are welcome.

- **Bugs and ideas:** open an [issue](https://github.com/marcelhoffs/anchor/issues/new/choose). For anything larger than a small fix, open an issue before you start on a pull request, so we can agree on the approach first.
- **Security problems:** don't open an issue; see [SECURITY.md](SECURITY.md).

## Development

You need Go (the version in `go.mod`); no C compiler, since the SQLite driver is pure Go.

```sh
go vet ./...
go test ./...
ANCHOR_DATA_DIR=./data ANCHOR_BASE_URL=http://localhost:8080 \
ANCHOR_SECRET_KEY=$(openssl rand -base64 32) go run ./cmd/anchor serve
```

CI runs `go vet` and `go test` on every pull request; both need to pass.

## Pull requests

- Keep each one to a single change, with tests for new behaviour.
- New user-facing text needs a translation in every language Anchor ships (English, Dutch, Spanish and French). If you can't translate it, say so in the pull request.
- Database changes go in a new migration; never edit one that has been released.
- Update the docs in `docs/` when you change how something works or is configured.

## License

Anchor is licensed under the [AGPL-3.0](LICENSE). By sending a pull request you agree that your contribution is licensed under it too.
