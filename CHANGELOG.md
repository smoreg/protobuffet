# Changelog

Notable changes, newest first. Versions follow [semver](https://semver.org);
until 1.0 the API and the on-disk layout may still change between minors.

## Unreleased

### Added

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

- The container image no longer compiles buf from source: that needed more
  memory than a small runner has and failed the build.

- Path traversal was possible in principle through the Composer, npm and PyPI
  download handlers; owner, name and version are now validated.
- Publishing a Go module at v2 or above without the `/vN` path suffix produced
  a module the go command downloads and then refuses.
- Re-publishing a version with the same schema but different generated code
  silently kept the old code; it is now refused with the difference named.
