#!/usr/bin/env bash
# Usage: release-notes.sh <tag> <owner/repo>
set -euo pipefail

tag=$1
repo=$2
prev=$(git describe --tags --abbrev=0 HEAD 2>/dev/null || true)

if [[ -n "$prev" ]]; then
  echo "## Changes since $prev"
  range="$prev..HEAD"
else
  echo "## $tag"
  range=HEAD
fi
echo
git log --no-merges --pretty='- %s' "$range"
cat <<EOF

## Install

\`\`\`sh
os=\$(uname -s | tr '[:upper:]' '[:lower:]')
arch=\$(uname -m); case \$arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
curl -fsSLo ecs-deploy "https://github.com/$repo/releases/download/$tag/ecs-deploy-\$os-\$arch"
chmod +x ecs-deploy && ./ecs-deploy --version
\`\`\`

GitHub Action: \`uses: $repo@$tag\`
EOF
if [[ -n "$prev" ]]; then
  echo
  echo "**Full diff**: https://github.com/$repo/compare/$prev...$tag"
fi
