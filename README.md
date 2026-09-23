# CloudForge

CloudForge is an incremental learning project for durable background job processing. It will grow from a Go API and PostgreSQL worker into a distributed platform. **Current phase: V0 foundation.** There is no job API or worker yet.

The [roadmap](docs/roadmap.md) records the planned V0–V13 progression. Features are added only when their purpose and failure modes can be demonstrated.

## Prerequisites

- Go 1.27 or newer (the version declared by `go.mod`)
- Docker Desktop with Docker Compose and a working Linux container backend
- PowerShell 7 or Windows PowerShell 5.1
- Git

On a Windows machine without WSL or Docker Desktop, enable WSL 2 from an elevated PowerShell terminal with `wsl --install`, restart Windows, then install and start Docker Desktop. The V0 Compose service needs Docker's Linux container backend. Install Go from [go.dev/dl](https://go.dev/dl/) and open a new terminal so `go` is on `PATH`.

## Local setup on Windows

Run these commands in the repository root:

```powershell
Copy-Item .env.example .env
docker compose up -d --wait db
docker compose ps
. .\scripts\load-env.ps1
go run ./cmd/dbcheck
go run ./cmd/migrate up
go run ./cmd/migrate status
go test ./...
go test -tags=integration ./...
```

The `.env` file contains local development credentials and is ignored by Git. Docker Compose reads it automatically for PostgreSQL settings. Go reads **process environment variables**, so dot-source `scripts/load-env.ps1` in each new PowerShell session before running Go commands. `DATABASE_URL` is required; `APP_ENV` defaults to `development`. If you change the PostgreSQL user, password, database, or published port, update `DATABASE_URL` to match. The example uses a local development password and `sslmode=disable` only for host-to-local-container traffic.

The Go process runs on Windows and connects to the published PostgreSQL port through `localhost`. A future Go container will instead use Docker's internal DNS name `db` and port `5432`; `localhost` inside that container would refer to the container itself.

## Everyday commands

```powershell
docker compose up -d --wait db        # start PostgreSQL and wait for health
docker compose ps                     # check the health status
go run ./cmd/dbcheck                  # verify a Go-to-PostgreSQL connection
go run ./cmd/migrate up               # apply all pending migrations
go run ./cmd/migrate down             # roll back one migration
go run ./cmd/migrate status           # list migration state
go test ./...                         # tests that need no database
go test -race ./...                   # requires CGO and a supported C compiler on Windows
go test -tags=integration ./...       # tests requiring PostgreSQL and migrations
docker compose down                   # stop containers; keep data volume
```

The commands that run Go require `DATABASE_URL` in the current terminal. The integration test is behind a build tag so ordinary `go test ./...` does not unexpectedly require PostgreSQL. Windows race detection also needs `CGO_ENABLED=1` and a C compiler such as MinGW-w64; the basic V0 test command needs neither. Never use `docker compose down -v` for a normal shutdown: `-v` deletes the persistent development database volume.

To check persistence, apply the migration, run `docker compose down`, then `docker compose up -d --wait db` and `go run ./cmd/migrate status`. The `cloudforge` schema should still be marked applied.

## Migrations

[Goose](https://github.com/pressly/goose) runs versioned SQL migrations through our small Go command, so Windows developers need no separate migration executable. V0's first migration creates only the `cloudforge` PostgreSQL schema, a namespace for later application tables. It is a real reversible change, so we can test up/down/up without adding a premature jobs table or a no-op migration. Goose records applied versions in its own table. Each `down` rolls back one version. Raw `psql` scripts would need us to build version tracking ourselves; `golang-migrate` is a sound alternative but would not materially simplify this V0 workflow. Migrations are applied explicitly in development and, later, against a fresh database in CI. Application startup does not silently change the schema.

After `go run ./cmd/migrate up`, you can exercise rollback and reapply:

```powershell
go run ./cmd/migrate down
go run ./cmd/migrate up
go test -tags=integration ./...
```

## Current architecture

```text
Go dbcheck / migration commands (Windows host)
                 |
                 | localhost:POSTGRES_PORT
                 v
        PostgreSQL (Docker Compose)
                 |
                 v
       persistent Docker volume
```

The Go module identifies the code's import path and pins its dependencies. Without it, builds would not be reproducible. Environment variables keep local settings outside the source code; missing `DATABASE_URL` fails clearly. `pgxpool` reuses PostgreSQL connections instead of opening one for every operation, verifies reachability with `Ping`, and closes when the command ends. Contexts bound connection attempts so a failed database does not hang indefinitely. The Compose healthcheck reports when PostgreSQL accepts connections; without it, a running container could be mistaken for a ready database. The volume keeps data when the container is stopped or recreated; without it, container replacement would erase the database. Migrations track schema versions so each developer and CI database can reach the same structure.

V1 will add a job API and one polling worker. It will intentionally show that a worker crash leaves a job stuck in `running`, providing a concrete reason to add leases and recovery later.
