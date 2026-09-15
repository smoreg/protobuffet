# Changelog

Notable changes, newest first. Versions follow [semver](https://semver.org);
until 1.0 the API and the on-disk layout may still change between minors.

## 0.1.1 — 2026-09-15

### Fixed

- The CLI did not build for Windows: the data-directory lock uses flock, which
  the Windows syscall package does not have. The lock is now Unix-only, with a
  stand-in that refuses to start the server elsewhere, and CI cross-compiles
  every released platform.

## 0.1.0 — 2026-09-15

First release. Everything below is new.

### Added

- `/readyz` and `/version`; readiness checks the data volume, the
  compatibility gate and package-name conflicts.
- `BUFFET_LOG_LEVEL`, a container `HEALTHCHECK`, CronJobs for collection and
  backups, a ServiceMonitor, and a production checklist.

- `buffet vendor`: schema dependencies between modules of your own registry,
  without buf.build. It downloads each dependency's schema archive and writes
  a buf workspace that makes the imports resolve.
- `buffet whoami` and a `/api/v1/whoami` endpoint, so CI can check a
  credential before it starts building rather than at publish time.
- `-json` output for `list`, `info`, `whoami` and `publish`.
- The client retries a network error or a 502/503/504 up to three times, and
  `BUFFET_TIMEOUT` sets its timeout.
- Write tokens can be limited to a set of modules
  (`modules: ["team1/*"]`), so a leaked CI credential cannot publish under
  another team's name.

- Python packages: wheels served through a PEP 503 index (`pip install
  --index-url`), with PEP 440 version mapping.
- `go get` discovery: the registry answers `?go-get=1`, so consumers need only
  `GOPRIVATE` instead of `GOPROXY` on every machine.
- Yanking: hide a bad release from listings while keeping it downloadable for
  lock files that already pin it.
- Prometheus metrics on `/metrics` and a request id on every response.
- Kubernetes manifests and a name-resolution guide in `docs/kubernetes.md`.
- `buffet label`, `buffet yank`, `buffet unyank`.

### Changed

- Uploads are streamed to disk and hashed in one pass instead of being held in
  memory: four concurrent publishes of large artifacts no longer need more RAM
  than the pod is allowed.

- The compatibility gate is now `off` / `best-effort` / `required`, defaulting
  to `required`: the server refuses to start without buf and refuses a publish
  it could not verify.
- Compatibility is checked against the preceding version rather than the
  highest one, so a patch on an older line is compared with the right release.
- Publishing stages a version in a temporary directory and renames it into
  place; artifact digests are computed by the server.
- Reads are served from an in-memory catalog instead of walking the data
  directory on every request.

### Fixed

- Two concurrent publishes could both claim the same package name: the check
  released its lock before writing, so the collision only surfaced at the next
  restart, where it stopped the registry from starting at all. Names are now
  reserved atomically, and a conflict already on disk is logged instead of
  refusing to start.
- Python package ownership was tracked under the raw name while pip matches
  the PEP 503 normalized one, so `Acme.Catalog_Proto` could shadow
  `acme-catalog-proto` published by another module. Composer and npm names are
  now compared case-insensitively for the same reason.
- The data directory takes an exclusive lock: `buffetd -gc` against a running
  registry used to interleave index writes and leave the in-memory catalog
  serving versions whose files were gone. Collection now goes through
  `POST /api/v1/gc`.
- Validation errors returned 500 instead of 400.
- Authorization middleware reassigned its captured scope, so the first request
  to `/api/v1/whoami` changed how that route behaved for every later one.
- Scopes now nest: a write token can read.
- npm `dist-tags.latest` pointed at the highest version including prereleases,
  so publishing an RC silently made it the default install for everyone.
  Latest is now the newest stable release, with prereleases under `next`.
- Python pseudo-versions dropped the commit digest, so two revisions in the
  same second produced one wheel name. The digest moves to the PEP 440 local
  segment.
- A hand-written tag such as `v0.0.0-alpha-1` was mistaken for a generated
  pseudo-version, which hid it from `@v/list` and exposed it to retention.
- The audit log recorded `X-Forwarded-For` and `X-Request-Id` verbatim, so a
  client could forge both. They are only believed behind `trusted_proxy`.
- A publish carrying no artifacts was accepted, burning a version number on a
  release nobody can install.
- The container image no longer compiles buf from source: that needed more
  memory than a small runner has and failed the build.

- Path traversal was possible in principle through the Composer, npm and PyPI
  download handlers; owner, name and version are now validated.
- Publishing a Go module at v2 or above without the `/vN` path suffix produced
  a module the go command downloads and then refuses.
- Re-publishing a version with the same schema but different generated code
  silently kept the old code; it is now refused with the difference named.
