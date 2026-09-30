#!/usr/bin/env bash
# Usage: ./build.sh [os/arch]   e.g. ./build.sh linux/amd64
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
commit=$(git rev-parse -q --short --verify HEAD || echo unknown)
[[ -n "$(git status --porcelain 2>/dev/null)" ]] && commit+="-dirty"
ldflags="-X main.commit=dev-$commit -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

out=ecs-deploy
if [[ $# -gt 0 ]]; then
  export GOOS=${1%/*} GOARCH=${1#*/}
  out="ecs-deploy-$GOOS-$GOARCH"
fi
CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$out" .
"./$out" --version 2>/dev/null || true
echo "Built ./$out"
