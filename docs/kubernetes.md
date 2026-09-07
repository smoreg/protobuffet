# Running protobuffet in Kubernetes

Manifests are in [`deploy/k8s`](../deploy/k8s) (kustomize). This document is
about the part that actually decides whether the thing works: how names
resolve for `go`, `pip`, `composer` and `npm`.

```sh
kubectl -n schema-registry create secret generic protobuffet-config \
  --from-file=buffetd.yaml=./buffetd.yaml
kubectl apply -k deploy/k8s
```

## The hostname is the contract

Pick one externally reachable hostname and never change it:

```
schemas.example.com
```

Go module paths start with it (`schemas.example.com/gen/go/acme/catalog`), so
it ends up inside every consumer's `go.mod`. Renaming it later invalidates
every published module path, and versions are immutable.

Two consequences:

- **The registry must answer on port 443 under that name.** A Go module path
  cannot contain a port, so a registry on `:8080` cannot be reached by module
  path at all — consumers would have to set `GOPROXY` by hand everywhere.
- **`base_url` must equal it.** Composer, npm and PyPI metadata carry absolute
  download URLs. Behind an ingress that rewrites `Host`, a wrong `base_url`
  produces metadata pointing at an address the client cannot reach.

## What consumers configure

**Go** — discovery does the work, so no `GOPROXY`:

```sh
export GOPRIVATE=schemas.example.com/*
go get schemas.example.com/gen/go/acme/catalog@v1.0.0
```

`GOPRIVATE` here means "do not send these paths to the public proxy or the
checksum database". The registry answers `?go-get=1` with a `go-import` meta
tag naming its own module proxy, and the go command follows it.

Never `GOSUMDB=off`: it turns off checksum verification for the whole build.

**Python** — in the CI image's `pip.conf`:

```ini
[global]
index-url = https://schemas.example.com/pypi/simple
extra-index-url = https://pypi.org/simple
```

**PHP** — one repository entry per project:

```sh
composer config repositories.protobuffet composer https://schemas.example.com/composer
```

**JavaScript** — scope the registry so only your packages go there:

```
@acme:registry=https://schemas.example.com/npm
```

## Builds inside the cluster

Use the same external hostname from in-cluster builds (Kaniko, Tekton, Argo),
not the Service DNS name. Metadata is generated from `base_url`, so a client
that arrived via `protobuffet.schema-registry.svc` would still be handed
`https://schemas.example.com/...` download URLs. Keeping one name avoids
split-horizon DNS and a second TLS certificate.

If the ingress controller does not allow hairpin traffic, add a CoreDNS
rewrite so the external name resolves to the Service inside the cluster — but
then the certificate must be valid for it too.

## TLS

`go`, `pip`, `composer` and `npm` all verify certificates, and none of them
have a convenient "trust this one host" switch worth using in production.

- Public DNS name: cert-manager with HTTP-01, or DNS-01 for a private zone.
- Internal CA: the CA certificate has to be in the trust store of **every**
  image that installs packages — Go uses the system pool, Python uses certifi,
  Composer uses the system pool, npm has its own `cafile`. This is the
  expensive option; a public name with DNS-01 is usually cheaper.

## Publishing from CI

```yaml
env:
  BUFFET_REGISTRY: https://schemas.example.com
  BUFFET_TOKEN: ${{ secrets.PROTOBUFFET_TOKEN }}   # write scope
steps:
  - run: buf lint
  - run: buffet check -against main      # fails the PR on a breaking change
  - run: buffet publish -label main      # on main
  - run: buffet publish -version ${GITHUB_REF_NAME}   # on a tag
```

The registry runs the same check on push, so a job that skips `buffet check`
still cannot publish a breaking change — it just finds out later.

## Reading permissions

Inside a private network, `public_read: true` is usually right: reads need no
credentials, and access is bounded by the NetworkPolicy and the ingress. For a
registry exposed to the internet, set `public_read: false` and hand out read
tokens — then mind that the go command only falls through `GOPROXY` entries on
404 and 410, not on 401, so either use `GOPRIVATE` discovery or put the
registry last in the list.

## Operations

- **Single writer.** `replicas: 1` with `strategy: Recreate`. The volume is
  ReadWriteOnce and the store is single-writer; scaling out needs the S3
  backend that does not exist yet.
- **Backups.** The PVC is the entire registry. Snapshot it, or run a CronJob
  that tars `/data` to object storage. Versions are immutable, so incremental
  backups only grow.
- **Restore.** Restore the volume, start the pod. The catalog is rebuilt from
  disk at startup, and it refuses to start if two modules claim the same
  package name — which is the check you want after a partial restore.
- **Memory.** The compatibility check unpacks two schema archives and runs
  buf; that is the peak. 512Mi is comfortable for schemas of a few megabytes.
