#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
version=$(tr -d '\r\n' < VERSION)
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "VERSION must be semver X.Y.Z, got '$version'" >&2
  exit 1
fi
commit=$(git rev-parse -q --verify HEAD || echo unknown)
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

rm -rf dist
mkdir dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
    go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.commit=$commit -X main.buildDate=$build_date" \
      -o "dist/ecs-deploy-$os-$arch" .
done
(cd dist && shasum -a 256 ecs-deploy-* > SHA256SUMS)
echo "Built $version into dist/"
