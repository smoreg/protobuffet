# Contributing

Patches, bug reports and questions are all welcome.

## Get it running

```sh
git clone https://github.com/smoreg/protobuffet
cd protobuffet
make build
BUFFET_ADMIN_TOKEN=dev ./bin/buffetd -data /tmp/protobuffet-data
```

In another shell:

```sh
export BUFFET_REGISTRY=http://localhost:8080 BUFFET_TOKEN=dev
./bin/buffet publish -config examples/catalog -version v0.1.0 -label main
./bin/buffet list
```

## Before opening a pull request

```sh
make lint   # go vet plus a gofmt check
make test   # unit and integration tests, no external tools needed
make e2e    # the real go/pip/composer round trip
```

`make e2e` needs `buf`, `protoc` and `protoc-gen-go`:

```sh
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
brew install protobuf          # or: apt install protobuf-compiler
```

## Where things live

| Path | What it does |
|---|---|
| `cmd/buffet` | the CLI |
| `cmd/buffetd` | the server |
| `internal/registry` | modules, versions, filesystem storage |
| `internal/pack` | zip, tarball, Go module zip, Python wheel |
| `internal/publish` | generation and packing on the client side |
| `internal/gomodproxy` `internal/pypirepo` `internal/composerrepo` `internal/npmrepo` | one package-manager protocol each |
| `internal/api` | HTTP routes, auth, the browsing UI |
| `internal/buftool` | wrapper around the buf binary |

## Adding a package ecosystem

Every ecosystem is the same six steps. Follow how Python was added if you want
a worked example:

1. `internal/pack` — build the archive format (wheel, gem, jar…).
2. `internal/registry/module.go` — add the `Target` constant.
3. `internal/registry/version.go` — map registry versions to that ecosystem's
   version syntax, if it differs from semver.
4. `internal/config/module.go` — add the target struct and its validation.
5. `internal/publish/publish.go` — generate, pack, return an `Artifact`.
6. `internal/<name>repo` — serve the protocol, then mount it in
   `internal/api/server.go` and add an install snippet to `internal/api/ui.go`.

Then a test in `internal/api/server_test.go` and a step in `e2e/e2e.sh` that
installs the package with the real client. The client is the specification; our
reading of the spec is not.

## Changelog

Add a line to the `## Unreleased` section of `CHANGELOG.md` in the same pull
request. Releases are cut by tagging `vX.Y.Z`; the tag builds binaries for
Linux, macOS and Windows plus a multi-arch container image.

## What gets a change merged quickly

- A test that fails without it.
- No new dependencies unless they carry real weight. The server depends on
  `golang.org/x/mod` and `gopkg.in/yaml.v3`, and that should stay boring.
- `make e2e` still passing, because that is what proves the protocols work.

## Out of scope

Code generation stays on the client. Proposals that move it into the server
turn a registry into a remote code execution service, and will be declined.
