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

## AWS credentials

The standard AWS SDK chain is used, same as the AWS CLI: `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`, `AWS_PROFILE`,
OIDC web identity, ECS task role or EC2 instance profile.
If your CI stores them under other names, map them to these variables.

GitHub Actions with OIDC (no stored keys):

```yaml
permissions:
  id-token: write
  contents: read
steps:
  - uses: aws-actions/configure-aws-credentials@v6
    with:
      role-to-assume: arn:aws:iam::123456789012:role/deploy
      aws-region: eu-central-1
  - uses: MaxTarasenko/ecs-swift-deploy@v0.1.0
    with: { cluster: my-cluster, service: my-service, task-definition: "${{ env.TD_ARN }}" }
```

GitHub Actions with stored keys:

```yaml
- uses: MaxTarasenko/ecs-swift-deploy@v0.1.0
  env:
    AWS_ACCESS_KEY_ID: ${{ secrets.PROD_AWS_KEY }}
    AWS_SECRET_ACCESS_KEY: ${{ secrets.PROD_AWS_SECRET }}
    AWS_REGION: eu-central-1
  with: { cluster: my-cluster, service: my-service, task-definition: "${{ env.TD_ARN }}" }
```

GitLab CI with stored keys:

```yaml
deploy:
  variables:
    AWS_ACCESS_KEY_ID: $PROD_AWS_KEY
    AWS_SECRET_ACCESS_KEY: $PROD_AWS_SECRET
    AWS_REGION: eu-central-1
  script:
    - ./ecs-deploy --cluster my-cluster --service my-service --task-definition "$TD_ARN"
```

GitLab CI with OIDC:

```yaml
deploy:
  id_tokens:
    AWS_TOKEN: { aud: sts.amazonaws.com }
  variables:
    AWS_ROLE_ARN: arn:aws:iam::123456789012:role/deploy
    AWS_REGION: eu-central-1
  script:
    - echo "$AWS_TOKEN" > "$CI_PROJECT_DIR/.aws-token"
    - AWS_WEB_IDENTITY_TOKEN_FILE="$CI_PROJECT_DIR/.aws-token" ./ecs-deploy --cluster my-cluster --service my-service --task-definition "$TD_ARN"
```

Binary, inline for one command or via a profile from `~/.aws/config`:

```sh
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
  ./ecs-deploy --region eu-central-1 --cluster my-cluster --service my-service --force-new-deployment

./ecs-deploy --profile prod --region eu-central-1 --cluster my-cluster --service my-service --force-new-deployment
```

Keys are not accepted as flags: arguments leak into process lists and CI logs.
Missing or expired credentials fail with `AWS_AUTH_ERROR`.

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
./build.sh                      # dev build for this machine: ./ecs-deploy
./build.sh linux/amd64          # cross-build: ./ecs-deploy-linux-amd64
bash scripts/build-release.sh   # all release binaries in dist/
```

Every push to `main` releases `v$(cat VERSION)` if that tag doesn't exist yet.
To release, bump `VERSION` (semver) in the same push. Existing releases are never overwritten.

## License

[MIT](LICENSE)
