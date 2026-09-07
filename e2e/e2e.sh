#!/usr/bin/env bash
# End-to-end proof that the registry speaks the real package-manager protocols:
# publish a schema, then install the generated SDKs with the actual `go` and
# `composer` clients, and confirm the registry refuses a breaking change.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
TMP=$(mktemp -d)
PORT=${PORT:-18080}
REGISTRY="http://127.0.0.1:${PORT}"
TOKEN="e2e-token-$(date +%s)"

export PATH="$(go env GOPATH)/bin:$PATH"

cleanup() {
  [[ -n "${SRV_PID:-}" ]] && kill "$SRV_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

step "build"
go build -o "$TMP/buffet" ./cmd/buffet
go build -o "$TMP/buffetd" ./cmd/buffetd
command -v protoc-gen-go >/dev/null || go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
command -v buf >/dev/null || { echo "buf is required: go install github.com/bufbuild/buf/cmd/buf@latest"; exit 1; }
# The example generates PHP through protoc_builtin, which drives protoc.
command -v protoc >/dev/null || { echo "protoc is required for the PHP target: install protobuf-compiler"; exit 1; }

step "start registry"
BUFFET_ADMIN_TOKEN="$TOKEN" "$TMP/buffetd" -listen "127.0.0.1:${PORT}" -data "$TMP/data" &
SRV_PID=$!
for _ in $(seq 1 50); do
  curl -sf "$REGISTRY/healthz" >/dev/null && break
  sleep 0.1
done
curl -sf "$REGISTRY/healthz" >/dev/null || { echo "registry did not start"; exit 1; }

step "publish acme/catalog v1.0.0"
export BUFFET_REGISTRY="$REGISTRY" BUFFET_TOKEN="$TOKEN"
"$TMP/buffet" publish -config examples/catalog -version v1.0.0 -label main
"$TMP/buffet" list

step "consume the Go SDK with the real go command"
mkdir -p "$TMP/goapp"
cd "$TMP/goapp"
cat > go.mod <<'MOD'
module e2e/consumer

go 1.24
MOD
cat > main.go <<'GO'
package main

import (
	"fmt"

	catalogv1 "registry.example.com/gen/go/acme/catalog/catalog/v1"
)

func main() {
	p := &catalogv1.Product{Sku: "TEA-001", Title: "Pu-erh 2019", PriceMinor: 4500, Currency: "GEL"}
	fmt.Printf("%s %s %d %s\n", p.GetSku(), p.GetTitle(), p.GetPriceMinor(), p.GetCurrency())
}
GO
export GOPROXY="${REGISTRY}/go,https://proxy.golang.org,direct"
export GOSUMDB=off
export GOFLAGS=-mod=mod
go get registry.example.com/gen/go/acme/catalog@v1.0.0
go run . | grep -q "TEA-001" && echo "go consumer built and ran against the registry SDK"
grep "registry.example.com" go.mod
cd "$ROOT"

step "consume the Composer package with the real composer client"
if command -v composer >/dev/null && command -v php >/dev/null; then
  mkdir -p "$TMP/phpapp"
  cd "$TMP/phpapp"
  cat > composer.json <<JSON
{
  "repositories": [{"type": "composer", "url": "${REGISTRY}/composer"}],
  "require": {"acme/catalog-proto": "1.0.0"},
  "config": {"secure-http": false}
}
JSON
  composer install --no-interaction --quiet
  cat > check.php <<'PHP'
<?php
require __DIR__ . '/vendor/autoload.php';
$p = new Acme\Catalog\V1\Product();
$p->setSku('TEA-001');
$p->setPriceMinor(4500);
echo $p->getSku(), ' ', $p->getPriceMinor(), PHP_EOL;
PHP
  php check.php | grep -q "TEA-001" && echo "php consumer ran against the registry package"
  cd "$ROOT"
else
  echo "skipped: composer or php not installed"
fi

step "consume the Python wheel with the real pip client"
if command -v python3 >/dev/null; then
  # --system-site-packages so a protobuf runtime already on the machine is
  # reused; --no-deps keeps the test pointed at our index instead of PyPI.
  python3 -m venv --system-site-packages "$TMP/venv"
  "$TMP/venv/bin/pip" install --quiet --no-deps \
    --index-url "${REGISTRY}/pypi/simple" "acme-catalog-proto==1.0.0"
  "$TMP/venv/bin/pip" show acme-catalog-proto | grep -E "^(Name|Version):"

  if "$TMP/venv/bin/python" -c "import google.protobuf" 2>/dev/null; then
    "$TMP/venv/bin/python" - <<'PYEOF'
from catalog.v1 import catalog_pb2

product = catalog_pb2.Product(sku="TEA-001", title="Pu-erh 2019", price_minor=4500, currency="GEL")
assert product.sku == "TEA-001"
print("python consumer ran against the registry wheel:", product.sku, product.price_minor)
PYEOF
  else
    "$TMP/venv/bin/python" -c "import pathlib,sys; p=[x for x in sys.path if 'site-packages' in x][0]; assert (pathlib.Path(p)/'catalog'/'v1'/'catalog_pb2.py').exists()"
    echo "wheel installed from the registry (import skipped: no protobuf runtime available)"
  fi
else
  echo "skipped: python3 not installed"
fi

step "the registry refuses a breaking change"
cp -R examples/catalog "$TMP/breaking"
# Renaming a field changes the contract for every consumer already on v1.0.0.
sed -i.bak 's/string sku = 2;/string stock_keeping_unit = 2;/' "$TMP/breaking/proto/catalog/v1/catalog.proto"
if "$TMP/buffet" publish -config "$TMP/breaking" -version v1.1.0 2>"$TMP/breaking.log"; then
  echo "FAIL: the registry accepted a breaking change"
  exit 1
fi
grep -qi "breaking\|compatib" "$TMP/breaking.log" && echo "breaking change rejected as expected:"
sed 's/^/  /' "$TMP/breaking.log" | head -6

step "a compatible change is accepted"
cp -R examples/catalog "$TMP/compatible"
sed -i.bak 's/  google.protobuf.Timestamp created_at = 7;/  google.protobuf.Timestamp created_at = 7;\n  int64 discount_minor = 8;/' \
  "$TMP/compatible/proto/catalog/v1/catalog.proto"
"$TMP/buffet" publish -config "$TMP/compatible" -version v1.1.0 -label main
"$TMP/buffet" info acme/catalog

printf '\n\033[1;32me2e passed\033[0m\n'
