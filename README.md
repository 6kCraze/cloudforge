# CloudForge

CloudForge is an incremental learning project for durable background job processing. **Current phase: V1: one Go API, one PostgreSQL-backed polling worker, and a deterministic synthetic workload.** PostgreSQL is the authoritative source of truth for every job lifecycle transition.

The [roadmap](docs/roadmap.md) records the planned V0-V13 progression. [V1 architecture](docs/v1-architecture.md) explains the request flow, lifecycle, atomic claim transaction, and intentional crash limitation.

## Prerequisites

- Go 1.27 or newer
- Docker Desktop with Docker Compose and a working Linux container backend
- PowerShell 7 or Windows PowerShell 5.1
- Git

## Local setup on Windows

Run these commands in the repository root:

```powershell
Copy-Item .env.example .env
docker compose up -d --wait db
. .\scripts\load-env.ps1
go run ./cmd/migrate up
go test ./...
go test -tags=integration ./...
```

The `.env` file contains local development credentials and is ignored by Git. Docker Compose reads it for PostgreSQL, while Go reads process environment variables. Dot-source `scripts/load-env.ps1` in each new PowerShell session before starting a Go command.

## Run V1

Open two PowerShell terminals in the repository root and load the environment in each.

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
docker compose up -d --wait db
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

RabbitMQ is intentionally absent in V1. When introduced later it will deliver work, while PostgreSQL remains canonical job state. Redis remains deferred until measurements show a concrete need.
