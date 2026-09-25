# CloudForge

CloudForge is an incremental learning project for durable background job processing. **Current phase: V2.3 outbox dispatch with the V1 polling worker unchanged.** Job creation atomically records both the queued job and its publication intent in PostgreSQL. A separate dispatcher reliably publishes that intent to RabbitMQ with at-least-once semantics. PostgreSQL remains authoritative for every job lifecycle transition.

The [roadmap](docs/roadmap.md) records the planned V0-V13 progression. [V1 architecture](docs/v1-architecture.md) explains the current worker flow and intentional crash limitation. [V2.1 RabbitMQ infrastructure](docs/v2-rabbitmq-infrastructure.md) documents the broker. [V2.2 transactional outbox](docs/v2-2-transactional-outbox.md) explains atomic job creation and publication intent. [V2.3 outbox dispatcher](docs/v2-3-outbox-dispatcher.md) explains confirmed RabbitMQ publication and its deliberate duplicate window.

## Prerequisites

- Go 1.27 or newer
- Docker Desktop with Docker Compose and a working Linux container backend
- PowerShell 7 or Windows PowerShell 5.1
- Git

## Local setup on Windows

Run these commands in the repository root:

```powershell
Copy-Item .env.example .env
docker compose up -d --wait db rabbitmq
. .\scripts\load-env.ps1
go run ./cmd/migrate up
go test ./...
go test -tags=integration ./...
```

The `.env` file contains local development credentials and is ignored by Git. Docker Compose reads it for PostgreSQL, while Go reads process environment variables. Dot-source `scripts/load-env.ps1` in each new PowerShell session before starting a Go command.

RabbitMQ's local management UI is available at [http://localhost:15672](http://localhost:15672). With an unchanged `.env.example`, sign in with username `cloudforge` and password `cloudforge_dev_only`. These credentials are only for local development. Starting `cmd/dispatcher` declares the V2.3 topology. Use the **Exchanges** and **Queues and Streams** tabs to inspect it. During V2.3, `cloudforge.jobs.v1` has zero consumers and its ready count increases as events publish.

## Run V2.3 locally

Open three PowerShell terminals in the repository root and load the environment in each.

Terminal 1:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/api
```

Terminal 2:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/worker
```

Terminal 3:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/dispatcher
```

Submit and inspect a job from a third terminal:

```powershell
$body = @{
  type = 'synthetic.sha256'
  payload = @{ text = 'hello CloudForge'; iterations = 100000 }
} | ConvertTo-Json

$accepted = Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/jobs `
  -ContentType 'application/json' -Body $body
$accepted

Invoke-RestMethod -Uri "http://localhost:8080/v1/jobs/$($accepted.id)" |
  ConvertTo-Json -Depth 5
```

`POST /v1/jobs` returns `202 Accepted` with a queued job ID. The worker atomically claims it, runs repeated SHA-256 hashing, and persists the digest and timing. Repeating the GET shows `queued`, `running`, then `succeeded` depending on timing.

Invalid input returns a structured `400` response:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "must be between 1 and 50000000",
    "field": "payload.iterations"
  }
}
```

A valid but unknown UUID returns `404`. A malformed UUID returns structured `400`.

## Intentional V1 crash behavior

V1 has no lease, heartbeat, or recovery loop. If the worker process is forcibly stopped after it claims a job, PostgreSQL correctly retains that job as `running`, but no worker may claim it again. This is a deliberate observed failure that V2 will solve.

To demonstrate it, submit a long job with `iterations = 50000000`, repeatedly GET it until it says `running`, then force-stop the worker process rather than using Ctrl+C. Restart the worker and GET the job again. It remains `running`; the replacement worker only claims `queued` jobs. The integration test `TestClaimWithoutCompletionRemainsRunning` proves the same behavior without requiring a manual process kill.

Ctrl+C is different: graceful shutdown stops new polling and lets the current claimed job finish before the worker exits.

## Everyday commands

```powershell
docker compose up -d --wait db rabbitmq
docker compose ps
. .\scripts\load-env.ps1
go run ./cmd/dbcheck
go run ./cmd/migrate up
go run ./cmd/migrate down
go run ./cmd/migrate status
go test ./...
go test -race ./...
go test -tags=integration ./...
docker compose down
```

The integration suite requires PostgreSQL with all migrations applied. The race detector on Windows requires CGO and a supported C compiler. If one is not installed, run it in the official Go container:

```powershell
docker run --rm -v "${PWD}:/src" -w /src golang:1.27 go test -race ./...
```

Do not use `docker compose down -v` for a normal shutdown because `-v` deletes the database volume.

## V1 components

```text
client -> Go API -> PostgreSQL jobs table <- polling Go worker
                         ^                         |
                         +---- result/failure ----+
```

- `cmd/api` accepts and reads jobs over HTTP.
- `cmd/worker` polls PostgreSQL and executes `synthetic.sha256` jobs.
- `internal/jobs` owns validation, lifecycle persistence, atomic claiming, and workload execution.
- `internal/httpapi` owns routes and structured HTTP errors.
- `internal/worker` owns polling and graceful shutdown behavior.
- `cmd/migrate` applies reversible Goose migrations.

RabbitMQ carries V2.3 `job.ready` notifications, while the V1 worker still polls PostgreSQL and does not consume them. PostgreSQL remains canonical job state. Redis remains deferred until measurements show a concrete need.

## V2.2 transactional outbox

`POST /v1/jobs` now performs one PostgreSQL transaction: it inserts the queued job, inserts one matching `job.ready` outbox event, and commits both together before returning `202 Accepted`. If either insert or the commit fails, neither row remains. The API response is unchanged and does not expose the internal event.

The outbox payload contains only the authoritative job ID:

```json
{"job_id":"28a3e40d-d025-4c22-b142-e359982e3f39"}
```

The event's ID and type are first-class columns. V2.3 can map them to RabbitMQ metadata and routing rather than duplicating them in the JSON body.

Every new outbox row intentionally has `published_at = NULL`. No dispatcher or RabbitMQ publisher exists in V2.2. The existing worker continues polling the `jobs` table and can complete the job while its outbox event remains unpublished. Job submission also continues to work when RabbitMQ is stopped.

## V2.3 confirmed publication

`cmd/dispatcher` reads a small ordered batch of unpublished events and processes it sequentially. It logs and leaves malformed events unpublished while continuing to valid later events in the batch. Each valid event is published as a persistent message to the durable `cloudforge.jobs` exchange with routing key `job.ready`, and the dispatcher waits for a RabbitMQ publisher confirmation. Mandatory publishing also detects a missing route. Only a routed broker ACK is followed by the conditional `published_at` update.

The dispatcher never keeps a PostgreSQL transaction or row lock open while waiting for RabbitMQ. It reconnects with capped exponential backoff and idempotently redeclares the durable exchange, queue, and binding. RabbitMQ has no CloudForge consumer in V2.3, so ready messages accumulate while the existing worker independently completes jobs by polling PostgreSQL.

RabbitMQ confirmation and the database update cannot be atomic. A crash between them leaves `published_at` NULL and causes the same event to publish again. The stable outbox UUID in RabbitMQ `message_id` makes this duplicate observable. This is at-least-once publication; V2.4 will make consumption safe in the presence of duplicates.
