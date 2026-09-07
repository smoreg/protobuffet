# Migrating from the Buf Schema Registry

Roughly a day's work for a handful of modules, most of it in consumer
repositories rather than in the registry.

## 1. Get the schemas out

```sh
buf export buf.build/myorg/catalog -o proto/
```

`buf export` writes the module's `.proto` files, including the ones it pulled
from dependencies. Keep your own protos and drop the vendored dependencies for
now; see step 4.

Put them in a repository of their own, with the `buf.yaml` you already have.

## 2. Describe the targets

Add a `buffet.yaml` next to `buf.yaml`. The Go module path is the decision
that is expensive to change later, because it ends up in every consumer's
`go.mod`:

```yaml
version: v1
module: myorg/catalog
proto: proto

targets:
  go:
    module: schemas.example.com/gen/go/myorg/catalog
    template: buf.gen.go.yaml
    out: gen/go
    require:
      - google.golang.org/protobuf v1.36.12
```

Point `go_package_prefix` in `buf.gen.go.yaml` at the same path.

## 3. Replace remote plugins

BSR runs generation plugins for you; protobuffet does not. Either install the
plugins locally:

```yaml
plugins:
  - local: protoc-gen-go
    out: gen/go
    opt: paths=source_relative
```

or keep using buf's remote plugins if buf.build is reachable from CI. The
registry does not care which produced the code.

PHP and Python generation go through `protoc_builtin`, which needs `protoc` on
PATH.

## 4. Deal with dependencies

`deps:` in `buf.yaml` still resolves through buf.build. Three options, in
order of how much they cost:

- **Keep resolving from buf.build** if CI can reach it. Nothing to do.
- **Vendor the dependency**: `buf export buf.build/googleapis/googleapis -o
  third_party/` and add the directory to `buf.yaml` as another module.
- **Publish it to your own registry** as an ordinary module, then vendor from
  there.

## 5. Publish and check

```sh
buffet publish -version v1.0.0 -label main
```

Publish the version consumers are already on first, so the compatibility gate
has a baseline to compare against.

## 6. Move the consumers

| Was | Becomes |
|---|---|
| `go get buf.build/gen/go/myorg/catalog/...` | `go get schemas.example.com/gen/go/myorg/catalog` |
| `@buf/myorg_catalog.bufbuild_es` | `@myorg/catalog` from your npm registry |
| BSR PyPI index | `--index-url https://schemas.example.com/pypi/simple` |

For Go, set `GOPRIVATE=schemas.example.com/*` in CI images; discovery does the
rest. See [kubernetes.md](kubernetes.md) for the resolution rules.

## 7. Turn off the old path

Keep the BSR module readable for a release or two: a consumer that has not
moved yet keeps building from the old SDK, and nothing forces a flag day.
