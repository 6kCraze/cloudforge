# Isolated local integration tests

All integration fixtures require `TEST_DATABASE_URL` and `TEST_RABBITMQ_URL`. They do not read or fall back to `DATABASE_URL` or `RABBITMQ_URL`. Missing or unsafe test targets fail before any connection, truncation, or queue purge.

The allowed targets are the local database **cloudforge_test**, PostgreSQL role **cloudforge_test**, RabbitMQ vhost **cloudforge_test**, and broker account **cloudforge_test**. URL overrides of database, user, or host are rejected. `host.docker.internal` is also allowed for a test running inside the cached Go image.

Use the existing PostgreSQL and RabbitMQ containers and volumes. No additional database or broker containers are needed. Run this setup from the repository root:

```powershell
docker compose up -d --wait --pull never db rabbitmq
Copy-Item .env.test.example .env.test  # first time only; do not overwrite a configured file
.\scripts\setup-test-infrastructure.ps1
. .\scripts\load-test-env.ps1
go test -p 1 ./internal/testenv
go test -p 1 -tags=integration ./internal/system -run '^TestV3TwoWorkersShareQueueSafely$' -count=1 -v -timeout=60s
```

`.env.test` is ignored by Git; `.env.test.example` contains only local test credentials. The setup script provisions the separate database and vhost, gives the test role no superuser/create-role/create-database privileges, and gives the broker user permissions only in the test vhost with no management tags. It migrates only the test database and restores the caller's `DATABASE_URL`. Provisioning uses the existing development PostgreSQL admin identity; it never truncates development tables. In a customized installation, verify that the test account has no preexisting grants in development namespaces.

Tests truncate `cloudforge.jobs` and `cloudforge.outbox_events` only inside the test database. Broker fixtures declare/purge the same queue name, `cloudforge.jobs.v1`, **inside the test vhost**. The development queue in `/` is independent. An advisory lock serializes fixtures sharing this test namespace; use `-p 1` and do not run application processes with test credentials while tests execute.

After changing shared fixture configuration, validate the affected packages sequentially:

```powershell
. .\scripts\load-test-env.ps1
go test -p 1 -tags=integration ./internal/database ./internal/jobs ./internal/dispatcher ./internal/worker ./internal/system -count=1 -timeout=120s
go vet -p 1 -tags=integration ./internal/testenv ./internal/database ./internal/jobs ./internal/dispatcher ./internal/worker ./internal/system
git diff --check
```

## Targeted integration race detector

Windows requires a supported C compiler for `-race`. If the official Go image is already cached, the following runs only the V3 integration test with one CPU and a 2 GiB memory ceiling. It mounts the existing module cache, disables dependency downloads and image pulls, and retains a temporary build cache to avoid repeated compilation. URLs below assume the localhost entries from `.env.test.example`.

```powershell
. .\scripts\load-test-env.ps1
$moduleCache = go env GOMODCACHE
$raceCache = Join-Path $env:TEMP 'cloudforge-linux-go-cache'
New-Item -ItemType Directory -Path $raceCache -Force | Out-Null
$testDbLinux = $env:TEST_DATABASE_URL.Replace('@localhost:', '@host.docker.internal:')
$testBrokerLinux = $env:TEST_RABBITMQ_URL.Replace('@localhost:', '@host.docker.internal:')
docker run --rm --pull=never --name cloudforge-v3-race --cpus=1 --memory=2g `
  -v "${PWD}:/src:ro" -v "${moduleCache}:/go/pkg/mod:ro" `
  -v "${raceCache}:/root/.cache/go-build" -w /src `
  -e GOPROXY=off -e GOTOOLCHAIN=local -e GOMAXPROCS=2 `
  -e "TEST_DATABASE_URL=$testDbLinux" -e "TEST_RABBITMQ_URL=$testBrokerLinux" `
  golang:1.27 go test -p 1 -race -tags=integration ./internal/system `
  -run '^TestV3TwoWorkersShareQueueSafely$' -count=1 -v -timeout=60s
```

If the image or modules are unavailable, report the race check as not executed rather than downloading them automatically. The container exits automatically; stop infrastructure afterward when finished:

```powershell
docker compose stop db rabbitmq
```

Do not delete Docker volumes. Inspect development jobs using the normal development account, and use the management UI's vhost selector to distinguish `/` from `cloudforge_test`. The restricted test account intentionally cannot log into that UI.
