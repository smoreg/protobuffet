# Self-hosting protobuffet

The server is one static binary with a data directory. Everything below is
about running it responsibly, not about making it work — that part is one
environment variable.

## Minimum

```sh
BUFFET_ADMIN_TOKEN=$(buffet token | head -1 | cut -d' ' -f2) buffetd
```

## Docker Compose

```sh
export BUFFET_ADMIN_TOKEN=<token from `buffet token`>
docker compose up -d
```

The compose file mounts a named volume at `/data`. That directory is the whole
state of the registry.

## Tokens

Credentials are random strings; the server stores only their SHA-256 digest.
Mint them with `buffet token`, which prints both halves:

```
token:  9f3c...                      # give this to CI
tokens:                              # paste this into buffetd.yaml
  - subject: ci
    sha256: 4b1e...
    scopes: [write]
```

Scopes are `read`, `write` and `admin`. A write token can also be limited to
the modules it may touch, which bounds what a leaked CI credential can reach:

```yaml
tokens:
  - subject: team-catalog-ci
    sha256: "..."
    scopes: [write]
    modules: ["catalog/*"]      # publish, label and yank, only under catalog/
```

Omit `modules` for a registry-wide credential. With `public_read: true` (the default)
reading needs no token at all, which is what makes `go get` work without
configuring credentials on every developer machine.

For a private registry set `public_read: false` and give consumers a `read`
token. ### The go command and 401

The go command falls through to the next entry in `GOPROXY` only on 404 and
410. A private registry answering 401 stops resolution for *every* module in
the build, including public ones. Two ways out:

```sh
# put the registry last, so the public proxy answers 404 for our paths first
export GOPROXY=https://proxy.golang.org,https://registry.example.com/go

# or skip GOPROXY entirely and let discovery do the work (registry on 443)
export GOPRIVATE=registry.example.com/*
```

The Go toolchain sends credentials from `~/.netrc`:

```
machine registry.example.com
  login buffet
  password <read token>
```

Composer uses `auth.json`:

```sh
composer config --global http-basic.registry.example.com buffet <read token>
```

pip takes credentials in the index URL:

```sh
pip install --index-url https://buffet:<read token>@registry.example.com/pypi/simple acme-catalog-proto
```

All three send HTTP basic auth, which the server accepts as an alternative to
`Authorization: Bearer`.

## Behind a reverse proxy

Composer and npm metadata contain absolute download URLs. By default the
server derives them from the incoming request, which is correct as long as the
proxy passes `Host` and `X-Forwarded-Proto`. If it rewrites either, pin the
value:

```yaml
base_url: "https://schemas.example.com"
```

nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 128m;   # publish uploads carry the SDKs
}
```

## TLS

Terminate it at the proxy. The server speaks plain HTTP on purpose: nobody
needs another certificate reload implementation.

Without TLS, Composer refuses plain-HTTP repositories unless the consumer sets
`"secure-http": false`, and that is a reasonable thing for it to insist on.

## Rotating credentials

Keep tokens in their own file so they can be replaced without a restart:

```yaml
tokens_file: /etc/protobuffet/tokens.yaml
```

```sh
# write the new file, then
kill -HUP $(pidof buffetd)      # or: kubectl exec deploy/protobuffet -- kill -HUP 1
```

A reload that fails validation is refused and the running set stays in place,
so a typo cannot lock everyone out. On the client side, CI should pass the
credential as `BUFFET_TOKEN` or point `BUFFET_TOKEN_FILE` at a mounted secret;
`buffet login` stores it in plain text and is meant for developer machines.

## Garbage collection

Every push without a tag creates a pseudo-version. Without a policy they
accumulate one per CI run:

```yaml
retention:
  keep_pseudo_versions: 50
  keep_pseudo_days: 90
```

While the registry is running, collect through the API. The data directory
takes an exclusive lock for the life of the process, so a second `buffetd -gc`
against the same volume refuses to start rather than interleaving its index
rewrites with a publish:

```sh
curl -X POST -H "Authorization: Bearer $ADMIN" https://schemas.example.com/api/v1/gc?dry_run=1
curl -X POST -H "Authorization: Bearer $ADMIN" https://schemas.example.com/api/v1/gc
```

Running in-process also refreshes the in-memory catalog; an external run would
leave the server serving versions whose files are gone.

With the registry stopped, the CLI does the same work and additionally removes
orphaned directories:

```sh
buffetd -gc -dry-run
buffetd -gc
```

Tagged versions and anything a label points at are never pruned.

## Audit log

Publishes, label moves and yanks are appended to `<data>/audit.log` as JSON
lines, and the last entries are available to an admin token:

```sh
curl -H "Authorization: Bearer $ADMIN" https://schemas.example.com/api/v1/audit?limit=50
```

## Backups

```sh
tar czf protobuffet-$(date +%F).tgz -C /var/lib/protobuffet .
```

The data directory is the only state. Versions are immutable, so an
incremental backup only ever grows; restoring is un-tarring it back.

Layout, if you ever need to look inside:

```
data/modules/<owner>/<name>/index.json                  versions and labels
data/modules/<owner>/<name>/versions/<version>/schema.zip
data/modules/<owner>/<name>/versions/<version>/go.zip
data/modules/<owner>/<name>/versions/<version>/composer.zip
data/modules/<owner>/<name>/versions/<version>/npm.tgz
```

## Module paths and ports

Go module paths cannot contain a port. A registry on `https://schemas.example.com`
can serve module path `schemas.example.com/gen/go/acme/catalog`, and consumers
need only `GOPRIVATE=schemas.example.com/*`. A registry on `:8080` cannot, and
its consumers must set `GOPROXY` explicitly.

## Toolchain on the publishing side

Publishing runs `buf` on the client. PHP and Python generation additionally
need the `protoc` binary, because their generators are built into protoc rather
than shipped as standalone plugins:

```sh
apt install protobuf-compiler     # or: brew install protobuf
```

## Compatibility checks

The server runs `buf breaking` against the latest published version of the
module. This needs the `buf` binary on PATH — the container image ships it. If
buf is missing, the server logs a warning at startup and publishes without the
check rather than failing every push.

To enforce checks in CI before a push, run them there too:

```sh
buffet check -config proto -against main
```

## Resource use

A registry serving a few hundred schema versions holds a few hundred megabytes
and idles at a few tens of MB of RSS. It fits on the smallest VM you have.
