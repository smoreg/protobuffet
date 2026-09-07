# protobuffet

A self-hosted registry for Protobuf schemas that serves generated SDKs as
**native packages**: a Go module proxy, a Composer repository and an npm
registry, all from one binary and one directory of files.

Publish a schema once; consumers install it with the package manager they
already use.

```sh
go get registry.example.com/gen/go/acme/catalog     # Go
composer require acme/catalog-proto                 # PHP
npm install @acme/catalog                           # JavaScript
```

No database, no object storage, no message broker. One Go binary, one data
directory, and [buf](https://github.com/bufbuild/buf) for the protobuf work.

## Why

Sharing `.proto` files between repositories has three usual answers, and two of
them are bad:

- **copy the files** — they drift, and nobody knows which copy is right;
- **a git submodule** — no versions, no compatibility guarantees, painful to
  update;
- **a registry** — versions, dependency resolution, and a gate that refuses
  changes which would break consumers already in production.

The registry answer exists as a hosted product (the Buf Schema Registry), but
self-hosting it needs an enterprise plan, and it is unreachable from some
networks. protobuffet is the small version of the same idea, Apache-2.0, that
you can run yourself.

## What it does

**Stores schema modules.** Immutable versions, semver ordering, mutable labels
(`main`, `stable`), pseudo-versions for untagged revisions.

**Serves SDKs as real packages.** The generated code is published into the
protocol each ecosystem already speaks, so no consumer needs a plugin, a
wrapper or a vendored copy:

| Ecosystem | Protocol served | Consumer command |
|---|---|---|
| Go | [module proxy](https://go.dev/ref/mod#goproxy-protocol) | `GOPROXY=<registry>/go go get <module>` |
| PHP | Composer repository | `composer config repositories.x composer <registry>/composer` |
| JavaScript | npm registry | `npm install --registry <registry>/npm` |

**Refuses breaking changes.** Every push is compared against the latest
published version with `buf breaking`. A rename that would break consumers is
rejected with the exact findings, before anything is stored.

**Keeps the schemas downloadable.** `GET /api/v1/modules/{owner}/{name}/versions/{version}/schema.zip`
is a valid `buf` input, so CI in any repository can check itself against the
registry without publishing.

## Quickstart

```sh
# 1. mint a credential
go install github.com/smoreg/protobuffet/cmd/buffet@latest
buffet token          # prints the token and the digest for the server config

# 2. run the registry
docker run -d -p 8080:8080 -v protobuffet:/data \
  -e BUFFET_ADMIN_TOKEN=<the token> ghcr.io/smoreg/protobuffet:latest

# 3. publish a schema module
export BUFFET_REGISTRY=http://localhost:8080 BUFFET_TOKEN=<the token>
buffet publish -config examples/catalog -version v1.0.0 -label main

# 4. consume it
export GOPROXY=http://localhost:8080/go,https://proxy.golang.org,direct GOSUMDB=off
go get registry.example.com/gen/go/acme/catalog@v1.0.0
```

Browse `http://localhost:8080` for the module list and copy-paste install
snippets.

## How it works

```
     schema repo                  registry                    consumers
  ┌────────────────┐        ┌──────────────────┐        ┌──────────────────┐
  │ buf.yaml       │ push   │ versions         │ go get │ Go services      │
  │ buffet.yaml    ├───────►│ schema archives  ├───────►│ (module proxy)   │
  │ proto/**.proto │        │ generated SDKs   │        ├──────────────────┤
  └────────────────┘        │ breaking check   │ require│ PHP apps         │
         │                  └──────────────────┘───────►│ (Composer repo)  │
         │ buffet publish            ▲                  ├──────────────────┤
         │  · buf lint               │ buf breaking     │ TypeScript       │
         │  · buf generate           │ against latest   │ (npm registry)   │
         └───────────────────────────┘                  └──────────────────┘
```

Code generation happens on the **client**, in the repository that owns the
schema, where the buf tooling already lives. The server stores what it is
given and serves it; it never executes generation plugins. That keeps the
server small and its attack surface narrow.

## Module configuration

`buffet.yaml` lives next to the schemas and says what to publish:

```yaml
version: v1
module: acme/catalog          # owner/name inside the registry
proto: proto                  # directory holding the .proto files

targets:
  go:
    module: registry.example.com/gen/go/acme/catalog   # the import path
    template: buf.gen.go.yaml
    out: gen/go
    require:
      - google.golang.org/protobuf v1.36.12

  composer:
    package: acme/catalog-proto
    template: buf.gen.php.yaml
    out: gen/php
    require:
      php: ">=8.1"
      google/protobuf: "^4.29"

  npm:
    package: "@acme/catalog"
    template: buf.gen.ts.yaml
    out: gen/ts
    dependencies:
      "@bufbuild/protobuf": "^2.2.0"
```

A complete, working example is in [`examples/catalog`](examples/catalog), and
it uses **local** generation plugins — no hosted plugin service, no network
beyond your own registry.

## Server configuration

Everything has a default; the shortest deployment is one environment variable:

```sh
BUFFET_ADMIN_TOKEN=<token> buffetd
```

See [`buffetd.example.yaml`](buffetd.example.yaml) for the full file and
[`docs/self-hosting.md`](docs/self-hosting.md) for TLS, reverse proxies,
backups and CI credentials.

## Commands

```
buffet publish   generate SDKs, pack them and push a version
buffet check     compare local schemas against the registry without publishing
buffet list      list modules
buffet info      show versions, targets and labels of one module
buffet login     store a token for a registry
buffet token     mint a token and print its config digest
```

## What this is not

Honest limits, so nobody is surprised in production:

- **No hosted code generation.** The client generates; the server stores.
  If your CI cannot run `buf`, this is not for you.
- **No web-based schema browser or generated docs.** The UI lists modules,
  versions and install commands. That is all it does.
- **No fine-grained permissions.** Tokens carry `read`, `write` or `admin`,
  registry-wide. Per-module ACLs are not implemented.
- **Single node.** Storage is a directory on disk. Run one instance, back the
  directory up. Horizontal scaling would need shared storage and is not
  implemented.
- **npm publishing is served, not verified end to end** in CI the way Go and
  Composer are — the protocol is implemented and unit-tested, but the e2e
  script only exercises the two ecosystems the author uses.

If you need hosted generation, org management, SSO and per-repository
permissions, use the [Buf Schema Registry](https://buf.build/product/bsr).
protobuffet deliberately covers the part most teams actually need.

## Development

```sh
make build   # binaries into ./bin
make test    # unit and integration tests
make e2e     # publish, then install with the real go and composer clients
make lint    # vet + gofmt check
```

The e2e script is the interesting one: it starts a registry, publishes the
example module, installs the Go SDK with the actual `go` command and the PHP
package with the actual `composer`, then proves the registry rejects a field
rename and accepts an added field.

Requires `buf` and `protoc-gen-go` on PATH; PHP and Composer are optional and
that part is skipped without them.

## License

Apache-2.0. See [LICENSE](LICENSE).
