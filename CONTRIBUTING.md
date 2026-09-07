# Contributing

Bug reports, questions and patches are welcome.

## Before opening a pull request

```sh
make lint    # go vet plus a gofmt check
make test    # unit and integration tests
make e2e     # the real go/composer round trip, needs buf and protoc-gen-go
```

## What makes a change easy to accept

- **A test that fails without the change.** The registry serves three
  package-manager protocols; a protocol change without a test is a change
  nobody can verify.
- **No new dependencies** unless they carry real weight. The server currently
  depends on `golang.org/x/mod` and `gopkg.in/yaml.v3`, and that is meant to
  stay unremarkable.
- **Compatibility with the published protocols.** Go, Composer and npm
  clients are the specification here, not our reading of it. If a change
  affects what they see, `make e2e` must still pass.

## Scope

protobuffet stores and serves schemas. Code generation belongs to `buf` and
lives on the client side; proposals that move generation into the server will
be turned down, because they turn the registry into an arbitrary code
execution service.
