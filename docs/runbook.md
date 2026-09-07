# Runbook

## Diagnosing what a client sees

| Symptom | Where to look |
|---|---|
| `go get` 404s | `curl -s https://schemas.example.com/go/<module>/@v/list`. Empty means the module was published under a different Go module path than the one being imported. |
| `go get` 401s | The go command falls through `GOPROXY` only on 404/410. Use `GOPRIVATE` discovery, or put the registry last in the list. |
| `go get: invalid version` | The module was published at v2+ without the `/v2` path suffix. |
| Composer cannot find the package | `curl -s https://schemas.example.com/composer/packages.json` for `available-packages`, then the `metadata-url` document. |
| Composer refuses plain HTTP | It insists on TLS unless the project sets `secure-http: false`. Terminate TLS at the proxy. |
| `pip` cannot find the package | Names are PEP 503 normalized: `Acme.Catalog` and `acme-catalog` are the same project. Check `/pypi/simple/`. |
| 409 on publish | Either the version exists with different content, or the change breaks compatibility. The body says which. |
| 503 on publish | Either too many concurrent publishes, or `breaking_check: required` and buf could not run. The log line says which. |

## Rotating a token

```sh
buffet token                       # mint, keep the secret for the client
# add the digest to the tokens file, remove the old entry
kubectl -n schema-registry exec deploy/protobuffet -- kill -HUP 1
```

A reload that fails validation is refused and the old set stays live.

## Restoring from a backup

```sh
kubectl scale deploy/protobuffet --replicas 0
# restore the volume from a snapshot
kubectl scale deploy/protobuffet --replicas 1
```

The catalog is rebuilt from disk at startup. If it refuses to start with
"package name already owned by another module", two modules on the restored
volume claim the same package name; the log names both.

## Reclaiming disk

```sh
buffetd -gc -dry-run     # what would go
buffetd -gc              # prune pseudo-versions, remove orphans
```

Releases and versions under a label are never pruned.

## Reading the audit log

```sh
curl -H "Authorization: Bearer $ADMIN" \
  'https://schemas.example.com/api/v1/audit?limit=200' | jq '.[] | select(.action=="publish")'
```

Or read `<data>/audit.log` directly; it is JSON lines, newest last.

## What to alert on

- `protobuffet_publishes_total{result="error"}` rising: publishes are failing
  for a reason that is not a breaking change.
- `protobuffet_publishes_total{result="throttled"}`: the concurrency limit is
  too low for the CI fleet.
- Readiness probe failing while the process is up: usually an unwritable data
  volume.
- Disk usage on the data volume: retention is not running.
