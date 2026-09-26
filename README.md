# CloudForge

CloudForge is an incremental learning project for durable background job processing. **Current phase: V2.5 failure-tested V2 architecture.** Job creation atomically records both the queued job and its publication intent in PostgreSQL. A dispatcher publishes that intent to RabbitMQ with at-least-once semantics, and a manual-ACK worker consumes it. PostgreSQL remains authoritative for every job lifecycle transition.

The [roadmap](docs/roadmap.md) records the planned V0-V13 progression. [V1 architecture](docs/v1-architecture.md) explains the original polling flow. [V2.1 RabbitMQ infrastructure](docs/v2-rabbitmq-infrastructure.md) documents the broker. [V2.2 transactional outbox](docs/v2-2-transactional-outbox.md) explains atomic job creation and publication intent. [V2.3 outbox dispatcher](docs/v2-3-outbox-dispatcher.md) explains confirmed publication. [V2.4 RabbitMQ worker](docs/v2-4-rabbitmq-worker.md) covers manual ACK and redelivery. The [V2 final architecture](docs/v2-final-architecture.md), [failure matrix](docs/v2-failure-matrix.md), and [V2.5 validation report](docs/v2-5-validation-report.md) state the tested guarantees and limits.

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

RabbitMQ's local management UI is available at [http://localhost:15672](http://localhost:15672). With an unchanged `.env.example`, sign in with username `cloudforge` and password `cloudforge_dev_only`. These credentials are only for local development. The dispatcher and worker idempotently declare the topology. Use **Queues and Streams** → `cloudforge.jobs.v1` to inspect Ready, Unacked, and Consumers.

## Run V2 locally

Open three PowerShell terminals in the repository root and load the environment in each.

Terminal 1:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/api
```

Terminal 2:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/dispatcher
```

Terminal 3:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/worker
```

Submit and inspect a job from a fourth terminal:

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

`POST /v1/jobs` returns `202 Accepted` with a queued job ID. The dispatcher publishes its outbox event. The worker receives it from RabbitMQ, atomically claims that specific job, runs repeated SHA-256 hashing, persists the digest and timing, and only then ACKs the delivery. Repeating the GET shows `queued`, `running`, then `succeeded` depending on timing.

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

## Intentional running-state crash behavior

V2.4 still has no lease, heartbeat, or recovery loop. If the worker process is forcibly stopped after it claims a job but before it records a terminal result and ACKs, RabbitMQ redelivers the message. PostgreSQL correctly retains the job as `running`, and the replacement worker conservatively refuses to re-execute it. This is a deliberate observed failure for the later recovery phase.

To demonstrate it, submit a long job with `iterations = 50000000`, repeatedly GET it until it says `running`, then force-stop the worker process rather than using Ctrl+C. RabbitMQ makes the unacknowledged message ready again. Restart the worker and GET the job again. It remains `running`; the replacement ACKs the redelivery without running SHA-256 again. The integration test `TestUnackedDeliveryIsRedeliveredAndRunningJobIsNotReexecuted` proves this with real RabbitMQ.

Ctrl+C is different: graceful shutdown cancels consumer intake and lets the current delivery reach a durable terminal state and ACK before the worker exits.

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

## Current components

```text
client -> Go API -> PostgreSQL jobs + outbox -> dispatcher -> RabbitMQ
                    ^                                          |
                    +------ manual-ACK worker <----------------+
```

- `cmd/api` accepts and reads jobs over HTTP.
- `cmd/dispatcher` publishes unpublished outbox events with confirms.
- `cmd/worker` consumes `job.ready` with manual ACK and executes `synthetic.sha256` jobs.
- `internal/jobs` owns validation, lifecycle persistence, atomic claim-by-ID, and workload execution.
- `internal/httpapi` owns routes and structured HTTP errors.
- `internal/worker` owns delivery policy, duplicate handling, reconnection, and graceful shutdown.
- `cmd/migrate` applies reversible Goose migrations.

RabbitMQ carries `job.ready` notifications and tracks Ready/Unacked delivery state. PostgreSQL remains canonical job state. Redis remains deferred until measurements show a concrete need.

## V2.2 transactional outbox

`POST /v1/jobs` now performs one PostgreSQL transaction: it inserts the queued job, inserts one matching `job.ready` outbox event, and commits both together before returning `202 Accepted`. If either insert or the commit fails, neither row remains. The API response is unchanged and does not expose the internal event.

The outbox payload contains only the authoritative job ID:

```json
{"job_id":"28a3e40d-d025-4c22-b142-e359982e3f39"}
```

The event's ID and type are first-class columns and map to RabbitMQ metadata and routing rather than being duplicated in the JSON body.

Every new outbox row starts with `published_at = NULL`. The V2.3 dispatcher later records the broker-confirmed publication time. Job submission continues to work when RabbitMQ is stopped because the API transaction only depends on PostgreSQL.

## V2.3 confirmed publication

`cmd/dispatcher` reads a small ordered batch of unpublished events and processes it sequentially. It logs and leaves malformed events unpublished while continuing to valid later events in the batch. Each valid event is published as a persistent message to the durable `cloudforge.jobs` exchange with routing key `job.ready`, and the dispatcher waits for a RabbitMQ publisher confirmation. Mandatory publishing also detects a missing route. Only a routed broker ACK is followed by the conditional `published_at` update.

The dispatcher never keeps a PostgreSQL transaction or row lock open while waiting for RabbitMQ. It reconnects with capped exponential backoff and idempotently redeclares the durable exchange, queue, and binding. RabbitMQ has no CloudForge consumer in V2.3, so ready messages accumulate while the existing worker independently completes jobs by polling PostgreSQL.

RabbitMQ confirmation and the database update cannot be atomic. A crash between them leaves `published_at` NULL and causes the same event to publish again. The stable outbox UUID in RabbitMQ `message_id` makes this duplicate observable. This is at-least-once publication; the V2.4 worker safely absorbs duplicates by consulting PostgreSQL and conditionally claiming only queued jobs.

## V2.4 manual-ACK consumption

The worker consumes one unacknowledged delivery at a time (`prefetch=1`). It validates the message, loads the authoritative job, and atomically changes only a queued job to running. It ACKs only after a terminal result is durable or after PostgreSQL proves execution is unnecessary or intentionally deferred. Malformed and unknown messages are logged and ACKed to prevent poison loops; temporary database handling errors are NACKed with requeue.

Broker or channel loss starts a small capped reconnect loop that restores topology, QoS, and consumption. Duplicate messages for succeeded or failed jobs do not run the workload again. A redelivery for a running job is ACKed without execution and leaves the job stuck until a later lease/recovery phase.

## V2.5 failure validation

V2.5 verifies broker, dispatcher, worker, database, startup-order, duplicate, and full-restart scenarios. A `202` job remains recoverable while RabbitMQ or the dispatcher is unavailable because PostgreSQL holds its outbox intent. Persistent Ready messages survived the tested clean broker restart. Success-before-ACK redelivery did not re-execute completed work.

The validation also found and fixed a bounded-backoff bug: a healthy RabbitMQ reconnect no longer resets worker or dispatcher delay while PostgreSQL remains unavailable. Both processes now increase retry delay up to their configured cap until the complete broker/database path succeeds.

The intentional limitation remains: a forced crash after `queued -> running` can leave a job stuck in `running`. V2 has no lease, heartbeat, recovery reaper, retry queue, or DLQ.
