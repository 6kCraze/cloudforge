# CI and engineering checks

One workflow, `.github/workflows/ci.yml`, runs on pushes, pull requests, and manual dispatch. Its action commits are immutable SHA pins verified against official v7 tags. It has read-only repository permissions and does not persist checkout credentials.

## Sequential checks

1. **quality** uses Ubuntu 24.04, Go from `go.mod`, and Node 24. `scripts/test.ps1` checks Go formatting (normalizing Windows CRLF for comparison), JavaScript syntax, PowerShell syntax/initialization safety, Go build, ordinary Go tests, and Go vet. There is no npm install, browser download, graphics build, or benchmark suite.
2. **integration** starts only after quality succeeds. The existing Compose definition creates ephemeral PostgreSQL and RabbitMQ services under the unique-to-this-runner `cloudforge-ci` project. Per-run generated passwords are masked before use and written only to ignored runner files. The restricted `cloudforge_test` account/database/vhost is provisioned through the same local setup script. Existing V3 test guards remain in force.
3. Packages run with `-p 1`, `GOMAXPROCS=2`, bounded timeouts, and no parallel package workload. Normal integration coverage includes transactions/outbox atomicity, confirmed publication, malformed-event handling, ACKs/duplicates, deterministic two-worker competition, thumbnail/batch routes, ownership fencing, retry exhaustion, and automatic recovery. Real killed-process tests are reserved for manual fault runs; the broker-stop and interactive browser sessions are not enabled by ordinary CI.
4. A manual dispatch with **fault-tests** enabled adds the existing two small real worker-process crash cases, then the guarded broker-interruption test. No new public checkpoint tags or duplicate workflow are needed. Tests that stop the broker must see no unrelated clients first.

Go's module/build cache is reused. Runtime configuration, credentials, uploaded files, and database volumes are not cached or uploaded as artifacts. Diagnostics show bounded container logs on failure; generated passwords are masked. Cleanup is guarded by `GITHUB_ACTIONS=true` and the exact ephemeral project name. Only that hosted runner's disposable project uses volume removal; local tooling only stops services and preserves volumes.

Compose file/env paths are absolute in CI so the broker fault test's Docker commands still select the same project when Go runs the test from a package subdirectory. Local setup retains the repository default project and existing named volumes.

## Local equivalents

```powershell
.\scripts\test.ps1
.\scripts\dev.ps1 infra
.\scripts\setup-test-infrastructure.ps1
.\scripts\test.ps1 -Integration
```

Optional process faults: `./scripts/test.ps1 -Integration -Faults`. Adding `-BrokerInterruption` explicitly permits the test to stop/start the shared local broker, subject to its idle-client guard. Stop application services first. Prefer the manually triggered workflow for these faults rather than repeatedly running them on the Windows development machine.

The live workflow must be reviewed and pushed before GitHub can execute it. Local syntax/expression validation is not evidence of a successful GitHub Actions run. A completed green hosted run for the reviewed commit is a release gate. Inspect the [review branch runs](https://github.com/6kCraze/cloudforge/actions?query=branch%3Areview%2Fv6-portfolio) and match their commit SHA; the dated local validation reports describe evidence available at those checkpoints.

References: [official setup-go inputs](https://github.com/actions/setup-go), [Compose environment selection](https://docs.docker.com/compose/how-tos/environment-variables/envvars/), and [actionlint](https://github.com/rhysd/actionlint).
