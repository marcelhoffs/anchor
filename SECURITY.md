# Security policy

## Reporting a vulnerability

Please don't open a public issue for a security problem. Report it privately through [GitHub's security advisories](https://github.com/marcelhoffs/anchor/security/advisories/new) instead.

Describe what you found, how to reproduce it, and which version you tested (shown in Anchor's footer, or by `anchor version`). You'll get an answer within a week. Once a fix is released, the advisory is published with credit to you, unless you'd rather stay anonymous.

## Supported versions

Only the latest release series gets security fixes. Anchor is still 0.x, so upgrade to the newest release before you report; the [upgrade guide](docs/deployment.md#upgrades) explains how.

## What counts

Anything that lets someone read, change or delete bookmarks they shouldn't, sign in as someone else, get past two-factor authentication, or make Anchor fetch from addresses that `ANCHOR_FETCH` doesn't allow. [Security measures](docs/how-it-works.md#security-measures) describes what Anchor promises.

Problems that need a misconfiguration the docs warn against, such as `ANCHOR_TRUSTED_PROXIES` set too wide or `ANCHOR_FETCH=all` on a server with untrusted users, are expected behaviour, not vulnerabilities.
