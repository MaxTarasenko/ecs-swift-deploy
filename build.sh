#!/usr/bin/env bash
# Usage: ./build.sh [os/arch]   defaults to this machine, e.g. ./build.sh linux/amd64
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
target=${1:-$(go env GOHOSTOS)/$(go env GOHOSTARCH)}
os=${target%/*}
arch=${target#*/}

commit=$(git rev-parse -q --short --verify HEAD || echo unknown)
[[ -n "$(git status --porcelain 2>/dev/null)" ]] && commit+="-dirty"
ldflags="-X main.commit=dev-$commit -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

out="build/ecs-deploy-$os-$arch"
mkdir -p build
CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$out" .
"./$out" --version 2>/dev/null || true
echo "Built ./$out"
