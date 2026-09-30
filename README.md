# ECS Swift Deploy

Updates an ECS service and waits for **that exact deployment** to finish.
Fails fast on the first stopped task and prints its reason and CloudWatch logs.

- Tracks the deployment ID returned by `UpdateService`, not overall service stability.
- Prints an AWS Console link for every new task as soon as it appears.
- Exits `1` on the first stopped task of the new deployment, even if ECS replaces it.
- Polls every 5s; success needs `COMPLETED`, all tasks on the new revision, no old tasks left.
- Read-only preflight before any write; `UpdateService` is never retried automatically.

## GitHub Action

```yaml
- uses: aws-actions/configure-aws-credentials@v6
  with:
    role-to-assume: arn:aws:iam::123456789012:role/deploy
    aws-region: eu-central-1

- uses: aws-actions/amazon-ecs-deploy-task-definition@v2
  id: register
  with:
    task-definition: task-definition.json  # no service/cluster: register only

- uses: MaxTarasenko/ecs-swift-deploy@v0.1.0
  with:
    cluster: my-cluster
    service: my-service
    task-definition: ${{ steps.register.outputs.task-definition-arn }}
```

Inputs: `cluster`, `service`, `task-definition`, `force-new-deployment`, `region`,
`timeout`, `interval`, `success-checks`, `log-lines`, `version`.
Outputs: `deployment-id`, `task-definition`, `elapsed-seconds`.
Runs on Linux and macOS runners (amd64/arm64).

## Binary

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
curl -fsSLo ecs-deploy "https://github.com/MaxTarasenko/ecs-swift-deploy/releases/latest/download/ecs-deploy-$os-$arch"
chmod +x ecs-deploy

./ecs-deploy --region eu-central-1 --cluster my-cluster --service my-service \
  --task-definition arn:aws:ecs:eu-central-1:123456789012:task-definition/my-service:42
```

Pin a version in CI: replace `latest/download` with `download/v0.1.0`.
Checksums are in `SHA256SUMS` next to the binaries.

Redeploy the current revision: `--force-new-deployment` instead of `--task-definition`.
All flags: `ecs-deploy --help`.

On success stdout gets one JSON line; everything else goes to stderr:

```json
{"status":"COMPLETED","deployment_id":"ecs-svc/…","task_definition":"arn:…","elapsed_seconds":82.4}
```

| Exit code | Meaning |
|---|---|
| 0 | Deployment completed |
| 1 | Deployment or task failed, AWS error, unsupported service |
| 2 | Bad arguments |
| 3 | AWS permission denied |
| 124 | Timeout |
| 130 | Interrupted |

Timeout or interrupt only stops waiting; the ECS deployment keeps going.

## IAM

- `ecs:DescribeServices`, `ecs:UpdateService`
- `ecs:DescribeTaskDefinition`, `ecs:ListTasks`, `ecs:DescribeTasks`
- `logs:GetLogEvents` on the task log streams
- `iam:PassRole` for the task and execution roles, if the task definition has them

## Limitations

- Only the ECS deployment controller with `ROLLING`, `REPLICA` services and `desiredCount > 0`.
- No CodeDeploy, blue/green, `EXTERNAL`, `DAEMON` or Classic ELB.
- The previous deployment must be `COMPLETED` before starting a new one.
- Not a lock: use a CI `concurrency` group per service.
- Any stopped task of the new deployment counts as a failure, including Spot interruptions and scale-in.

## Development

```sh
go test -race ./...
bash scripts/build-release.sh   # binaries in dist/
```

Every push to `main` releases `v$(cat VERSION)` if that tag doesn't exist yet.
To release, bump `VERSION` (semver) in the same push. Existing releases are never overwritten.

## License

[MIT](LICENSE)
