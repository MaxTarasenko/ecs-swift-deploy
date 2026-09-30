#!/usr/bin/env bash
set -euo pipefail

repo=${ACTION_REPO:-MaxTarasenko/ecs-swift-deploy}
tag=${INPUT_VERSION:-v$(tr -d '\r\n' < "$GITHUB_ACTION_PATH/VERSION")}

case "${RUNNER_OS:-$(uname -s)}" in
  Linux) os=linux ;;
  macOS | Darwin) os=darwin ;;
  *) echo "::error::Unsupported runner OS: ${RUNNER_OS:-unknown}"; exit 1 ;;
esac
case "${RUNNER_ARCH:-$(uname -m)}" in
  X64 | x86_64) arch=amd64 ;;
  ARM64 | arm64 | aarch64) arch=arm64 ;;
  *) echo "::error::Unsupported runner arch: ${RUNNER_ARCH:-unknown}"; exit 1 ;;
esac

file="ecs-deploy-$os-$arch"
dir="${RUNNER_TEMP:-/tmp}/ecs-swift-deploy/$tag"
bin="$dir/ecs-deploy"
if [[ ! -x "$bin" ]]; then
  mkdir -p "$dir"
  base="https://github.com/$repo/releases/download/$tag"
  curl -fsSL --retry 3 -o "$dir/$file" "$base/$file"
  curl -fsSL --retry 3 -o "$dir/SHA256SUMS" "$base/SHA256SUMS"
  (cd "$dir" && grep " $file\$" SHA256SUMS | shasum -a 256 -c -)
  mv "$dir/$file" "$bin"
  chmod +x "$bin"
fi
"$bin" --version

args=(--cluster "$INPUT_CLUSTER" --service "$INPUT_SERVICE"
  --timeout "$INPUT_TIMEOUT" --interval "$INPUT_INTERVAL"
  --success-checks "$INPUT_SUCCESS_CHECKS" --log-lines "$INPUT_LOG_LINES")
[[ -n "${INPUT_TASK_DEFINITION:-}" ]] && args+=(--task-definition "$INPUT_TASK_DEFINITION")
[[ "${INPUT_FORCE:-false}" == "true" ]] && args+=(--force-new-deployment)
[[ "${INPUT_WAIT_DRAIN:-false}" == "true" ]] && args+=(--wait-drain)
[[ -n "${INPUT_REGION:-}" ]] && args+=(--region "$INPUT_REGION")

result=$("$bin" "${args[@]}")
echo "$result"
{
  echo "deployment-id=$(jq -r .deployment_id <<<"$result")"
  echo "task-definition=$(jq -r .task_definition <<<"$result")"
  echo "elapsed-seconds=$(jq -r .elapsed_seconds <<<"$result")"
} >> "$GITHUB_OUTPUT"
