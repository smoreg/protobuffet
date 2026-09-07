# What the registry serves

Three package-manager protocols and one small HTTP API. This document is what
you need to debug a consumer that is not resolving.

## Go module proxy

Mounted at `/go`, implementing the [GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol):

| Endpoint | Returns |
|---|---|
| `/go/<module>/@v/list` | tagged versions, one per line |
| `/go/<module>/@v/<version>.info` | `{"Version":…, "Time":…}` |
| `/go/<module>/@v/<version>.mod` | the `go.mod`, read out of the module zip |
| `/go/<module>/@v/<version>.zip` | the module zip |
| `/go/<module>/@latest` | the highest version |

Module paths are case-encoded by the go command (`!` before a lowercased
capital); the server decodes them with `golang.org/x/mod/module`.

Pseudo-versions are resolvable but never listed in `@v/list`, because the go
command treats listed versions as releases and would prefer one over a tag.

Zips are built with `golang.org/x/mod/zip`, so they satisfy every rule the go
command checks on download.

## Composer repository

Mounted at `/composer`:

- `/composer/packages.json` — every package and version with an absolute
  `dist.url`, a `reference` (sha256) and a `shasum` (sha1);
- `/composer/dist/<owner>/<name>/<version>.zip` — the archive.

Consumers add one repository entry:

```json
{"repositories": [{"type": "composer", "url": "https://registry.example.com/composer"}]}
```

Composer rejects the leading `v` in versions, so `v1.2.3` in the registry is
published as `1.2.3`, and an untagged revision becomes `0.0.0-<timestamp>`,
which is a valid semver prerelease and orders correctly.

## npm registry

Mounted at `/npm`:

- `/npm/<package>` — the packument, with `dist-tags.latest` and a `dist.tarball`
  per version. Scoped names arrive URL-encoded (`@acme%2fcatalog`) and are read
  from the raw escaped path;
- `/npm/dist/<owner>/<name>/<version>.tgz` — the tarball, with every entry
  under the `package/` prefix npm requires.

## Registry API

| Method | Path | Scope |
|---|---|---|
| `GET` | `/api/v1/modules` | read |
| `GET` | `/api/v1/modules/{owner}/{name}` | read |
| `GET` | `/api/v1/modules/{owner}/{name}/versions/{version}` | read |
| `GET` | `/api/v1/modules/{owner}/{name}/versions/{version}/schema.zip` | read |
| `POST` | `/api/v1/modules/{owner}/{name}/versions` | write |
| `PUT` | `/api/v1/modules/{owner}/{name}/labels/{label}` | write |

`{version}` accepts a version, a label or `latest`.

A publish is `multipart/form-data`: a `manifest` field holding JSON, plus one
file part per artifact named by its file name (`schema.zip`, `go.zip`,
`composer.zip`, `npm.tgz`).

Status codes worth knowing:

- **201** published;
- **200** the same version with identical content already existed — a CI retry
  is not an error;
- **409** either the version exists with *different* content, or the push
  breaks compatibility; the body carries the `buf breaking` findings;
- **400** a digest in the manifest does not match the uploaded bytes.

## Versions

A publish takes its version from, in order: the `-version` flag, an exact git
tag on `HEAD`, or a generated pseudo-version
`v0.0.0-<utc timestamp>-<first 12 hex of the schema digest>` — the exact shape
the go command accepts.

Published versions are immutable. Labels are not: `-label main` moves the
label to the new version, which is how consumers follow a branch without
depending on a moving version number.
