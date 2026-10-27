# CloudForge

CloudForge is an incremental learning project for durable background job processing. **Current phase: V5 reliability and crash recovery.** Select one to four JPEG/PNG images, submit a durable batch, watch actual job states, preview thumbnails, and download PNG results. Each image uses the job/outbox → RabbitMQ → Go worker flow. V5 adds leased ownership, bounded retries, and automatic redispatch; synthetic SHA-256 jobs remain supported. PostgreSQL remains authoritative for every job lifecycle transition.

The [V4 image workflow](docs/v4-image-workflow.md) documents the browser, batch and single-image routes, limits, storage, and validation. V4 is released at `v0.4.0`. [V5 safe ownership](docs/v5-safe-ownership.md) covers leases and fencing; [V5 recovery and fault validation](docs/v5-recovery.md) covers bounded retries, redispatch, and crash testing. The development database was upgraded to migration 7 after explicit approval and a verified pre-upgrade archive listing.

The [roadmap](docs/roadmap.md) records the planned V0-V6 progression and optional backlog. [V1 architecture](docs/v1-architecture.md) explains the original polling flow. [V2.1 RabbitMQ infrastructure](docs/v2-rabbitmq-infrastructure.md) documents the broker. [V2.2 transactional outbox](docs/v2-2-transactional-outbox.md) explains atomic job creation and publication intent. [V2.3 outbox dispatcher](docs/v2-3-outbox-dispatcher.md) explains confirmed publication. [V2.4 RabbitMQ worker](docs/v2-4-rabbitmq-worker.md) covers manual ACK and redelivery. The [V2 final architecture](docs/v2-final-architecture.md), [failure matrix](docs/v2-failure-matrix.md), and [V2.5 validation report](docs/v2-5-validation-report.md) state the tested V2 guarantees. [V3 multiple-worker scaling](docs/v3-multiple-workers.md) explains competing consumers, deterministic concurrency validation, and the lightweight local demo.

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
```

The `.env` file contains local development credentials and is ignored by Git. Docker Compose reads it for PostgreSQL, while Go reads process environment variables. Dot-source `scripts/load-env.ps1` in each new PowerShell session before starting a Go command.

RabbitMQ's local management UI is available at [http://localhost:15672](http://localhost:15672). With an unchanged `.env.example`, sign in with username `cloudforge` and password `cloudforge_dev_only`. These credentials are only for local development. The dispatcher and worker idempotently declare the topology. Use **Queues and Streams** → `cloudforge.jobs.v1` to inspect Ready, Unacked, and Consumers.

## Run locally

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

Open [CloudForge](http://localhost:8080/) after starting the three processes. Choose or drop **one to four JPEG/PNG images**, click **Process images**, then preview and download completed PNG thumbnails. Saved batch links restore status after a reload. Progress counts actual PostgreSQL job states; polling runs about every two seconds and stops at completion. Use one worker normally.

Limits: **5 MiB per image, 21 MiB per batch request, 8 megapixels per image, and 8,192 pixels per side**. Configure the same absolute `STORAGE_ROOT` for API and worker, or leave it blank for the shared per-user cache directory. No Node.js or frontend build is required; Go embeds the HTML/CSS/JavaScript.

Existing databases need migrations **00004** and **00005** for images and batches, then **00006** and **00007** for V5 ownership and retries. They preserve existing jobs. The local development database for this checkout reached version 7 after an approved, backed-up upgrade; other databases must be checked and backed up before upgrading. See the [V5 upgrade record](docs/v5-recovery.md).

The synthetic SHA-256 API remains available. Submit and inspect a job from a fourth terminal:

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

## Historical V2 running-state crash behavior

V2.4 had no lease, heartbeat, or recovery loop. If the worker process was forcibly stopped after it claimed a job but before it recorded a terminal result and ACKed, RabbitMQ redelivered the message. PostgreSQL retained the job as `running`, and the replacement worker conservatively refused to re-execute it. V5 now recovers such jobs after their lease expires; the steps below describe the historical V2 demonstration.

On the historical V2 tag, the demonstration submitted a long job with `iterations = 50000000`, waited for `running`, then force-stopped the worker. RabbitMQ made the unacknowledged message ready again, but the replacement worker left the PostgreSQL job in `running`. The integration test `TestUnackedDeliveryIsRedeliveredAndRunningJobIsNotReexecuted` proved that V2 limitation with real RabbitMQ. Do not use that CPU-heavy demonstration for V5 smoke testing.

Ctrl+C is different: graceful shutdown cancels consumer intake and lets the current delivery reach a durable terminal state and ACK before the worker exits.

## Everyday commands

```powershell
docker compose up -d --wait db rabbitmq
docker compose ps
. .\scripts\load-env.ps1
go run ./cmd/dbcheck
go run ./cmd/migrate up
go run ./cmd/migrate status
go test ./...
go test -race ./...
. .\scripts\load-test-env.ps1
go test -p 1 -tags=integration ./... -timeout=120s
docker compose down
```

Integration fixtures require explicit isolated test URLs, a dedicated `cloudforge_test` database and RabbitMQ vhost, and restricted test accounts. They refuse development URLs before connecting. Follow [integration-test setup](docs/integration-testing.md) before running integration commands; never use the development database or normal broker queue for fixtures. The race detector on Windows requires CGO and a supported C compiler. If one is not installed, run it in the official Go container:

```powershell
docker run --rm --pull=never --cpus=1 --memory=2g -v "${PWD}:/src" -w /src golang:1.27 go test -p 1 -race ./...
```

Do not use `docker compose down -v` for a normal shutdown because `-v` deletes the database volume. Do not run a V5 down migration as a routine rollback: it can discard attempt history and cannot represent multiple retry-ready events under the older schema. Follow the [reviewed restore guidance](docs/v5-recovery.md).

## Current components

```text
client -> Go API -> PostgreSQL jobs + outbox -> dispatcher -> RabbitMQ
                    ^                                          |
                    +------ manual-ACK worker <----------------+
```

- `cmd/api` accepts and reads jobs over HTTP.
- `cmd/dispatcher` publishes unpublished outbox events with confirms and recovers expired jobs.
- `cmd/worker` consumes `job.ready` with manual ACK and executes thumbnail or `synthetic.sha256` jobs under leased attempt ownership.
- `internal/jobs` owns validation, lifecycle persistence, atomic claim-by-ID, and workload execution.
- `internal/httpapi` owns routes and structured HTTP errors.
- `internal/worker` owns delivery policy, duplicate handling, reconnection, and graceful shutdown.
- `cmd/migrate` applies Goose migrations; review V5 rollback constraints before any down migration.

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

## V3 multiple workers

V3 runs PostgreSQL and RabbitMQ in Docker while the API, dispatcher, and workers remain local Go processes. Start two workers with distinct identities in separate terminals:

```powershell
. .\scripts\load-env.ps1
$env:WORKER_ID = "worker-01"
go run ./cmd/worker
```

```powershell
. .\scripts\load-env.ps1
$env:WORKER_ID = "worker-02"
go run ./cmd/worker
```

Both consume `cloudforge.jobs.v1`. RabbitMQ distributes deliveries among available consumers; it does not broadcast every job to both. Prefetch remains one per worker. PostgreSQL's conditional `queued -> running` claim remains the execution guard if duplicate notifications reach different workers.

The V3 integration test uses two blocking processors to prove overlapping execution without CPU-heavy synthetic work. It also proves that two duplicate deliveries result in one execution and that stopping one worker leaves the other operational. Application containerization remains deferred until it materially helps reproducibility, deployment, or scaling.
