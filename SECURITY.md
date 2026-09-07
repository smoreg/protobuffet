# Security

## Reporting a vulnerability

Open a [private security advisory](https://github.com/smoreg/protobuffet/security/advisories/new)
rather than a public issue. Expect a first reply within a week.

## What the registry assumes

- **Authenticity comes from TLS.** The server speaks plain HTTP and expects a
  reverse proxy to terminate TLS. Digests in Composer, npm and PyPI metadata
  protect against corrupted transfers, not against a hostile network.
- **Tokens are bearer credentials.** Only their SHA-256 digest is stored; the
  server never has the plaintext after configuration. A leaked write token can
  publish new versions, and cannot rewrite existing ones — published versions
  are immutable.
- **Uploads are archives from an authenticated client.** They are stored as
  received and served back; the server does not execute anything from them.
  Extraction, which happens only for the compatibility check, rejects paths
  that escape the destination directory.
- **`public_read: true` is the default.** Anyone who can reach the port can
  read every schema. Set it to `false` for a private registry.

## Known limits

The server does not rate-limit, and it does not scan uploads. Put it behind a
proxy that does, if it is exposed to the internet.
