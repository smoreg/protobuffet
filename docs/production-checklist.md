# Production checklist

Work through this before the first team depends on the registry. Each item is
something that is cheap now and expensive after schemas are published.

## Decide once, cannot change later

- [ ] **Hostname.** Go module paths start with it and end up in every
      consumer's `go.mod`. It must resolve on **port 443**: a module path
      cannot carry a port. Changing it later invalidates every published Go
      module path.
- [ ] **Module naming.** `owner/name` where owner is a team or a domain, not a
      person. Package names per ecosystem follow from it and are claimed
      first-come.
- [ ] **`base_url`** equals the external hostname, or metadata points clients
      at an address they cannot reach.

## Configuration

- [ ] `breaking_check: required` — the default. Anything looser means a broken
      toolchain silently removes the only protection consumers have.
- [ ] `breaking_policy` — `strict` unless your team versions modules rather
      than proto packages.
- [ ] `public_read` — `true` inside a trusted network, `false` on the
      internet. If false, read the note about `go` and 401 in
      [self-hosting.md](self-hosting.md).
- [ ] `trusted_proxy: true` **only** behind a proxy that sets
      `X-Forwarded-For`; otherwise those values are client-supplied and end up
      in the audit log.
- [ ] `tokens_file` separate from the rest of the config, so credentials
      rotate with `SIGHUP` instead of a restart.
- [ ] Per-team write tokens carry `modules:` patterns. A leaked registry-wide
      token can publish under any name.
- [ ] `retention` set, or the volume fills with one pseudo-version per CI push.

## Deployment

- [ ] `replicas: 1`, `strategy: Recreate`. The store is single-writer and
      takes an exclusive lock; a second instance refuses to start.
- [ ] `fsGroup: 65532` on the pod, or the nonroot process cannot write the
      volume.
- [ ] Resource limits: 512Mi is comfortable. The compatibility check unpacks
      two schema archives and runs buf; that is the peak.
- [ ] Ingress `proxy-body-size` at least 256m, or publishes are truncated.
- [ ] TLS terminated at the proxy; every client verifies certificates.
- [ ] Backups of the data volume. It is the entire registry.
- [ ] `POST /api/v1/gc` on a schedule. Never `buffetd -gc` against a live
      volume — it will refuse, which is the point.

## Observability

- [ ] `/metrics` scraped. Alert on `protobuffet_publishes_total{result="error"}`
      rising, on `result="throttled"`, and on readiness failing while the
      process is up (usually a read-only volume).
- [ ] Disk usage on the data volume.
- [ ] Audit log shipped or backed up with the volume; it is the record of who
      published what.

## Before the first real publish

- [ ] Publish the version consumers already use, so the gate has a baseline.
- [ ] `buffet whoami` from CI, to fail early on a bad credential.
- [ ] `buffet check -against main` in the PR pipeline, so a breaking change is
      caught before the push rather than by the registry.
- [ ] One consumer repository migrated end to end, including a dependency
      update, before anyone else starts.

## Known limits

- Single node. HA needs a shared-storage backend that does not exist yet.
- Public schema dependencies (googleapis) are not proxied: export and publish
  them as modules of your own registry.
- Pruned pseudo-versions are deleted, not yanked; a lock file pinning one
  stops resolving. Pin tags in anything that must keep building.
