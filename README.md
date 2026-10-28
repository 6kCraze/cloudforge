# CloudForge — Reliable Distributed Job Processing

A Go-based background processing platform using PostgreSQL, RabbitMQ, transactional messaging, concurrent workers, and automatic crash recovery.

**Upload a small image batch. Follow its saved job states. Download the finished thumbnails.**

![CloudForge's real browser interface: an interactive infrastructure city](docs/images/overview.jpg)

Latest release: **[v0.5.0](https://github.com/6kCraze/cloudforge/releases/tag/v0.5.0)**. The V6 portfolio and tooling changes are under review on `review/v6-portfolio`; the `v0.6.0` release is pending. See the [actual hosted CI runs](https://github.com/6kCraze/cloudforge/actions?query=branch%3Areview%2Fv6-portfolio) for validation status. This is a local development application, with no hosted public demo.

## What it does

- Accepts one to four JPEG/PNG images and creates durable asynchronous jobs.
- Generates proportional PNG thumbnails, including transparent images, through RabbitMQ and Go workers.
- Shows actual PostgreSQL job states, worker identity, attempts, retry timing and available completion details.
- Recovers abandoned jobs with leases, bounded retries and automatic redispatch.
- Previews and downloads successful results; saved batch links restore status after reload.

The original synthetic SHA-256 job API remains supported. The SVG city explains six infrastructure components; its routes are **illustrative architecture**, not live broker telemetry.

![A real completed three-image batch with thumbnail previews](docs/images/workspace.jpg)

## Architecture

![CloudForge system design, including durable state, delivery, fenced ownership and recovery](docs/images/architecture.svg)

PostgreSQL is the **source of truth** for lifecycle and attempts. RabbitMQ delivers work notifications. One dispatcher handles outbox publication and runs an independent recovery loop; workers execute under leased PostgreSQL ownership. Outputs use local, attempt-specific paths.

| Mechanism | Why it matters |
| --- | --- |
| Transactional outbox | Job creation and delivery intent commit together, so accepting work does not require an available broker. |
| Publisher confirms + mandatory routing | The dispatcher marks an event published only after routed broker acceptance; the gap before its database update can still produce duplicates. |
| At-least-once delivery + atomic claims | Competing workers use PostgreSQL to elect the current owner even if notifications repeat. Manual ACK follows a durable processing decision; prefetch is one. |
| Leases, heartbeats and bounded retries | Crashed ownership expires. Recovery schedules up to three total attempts and atomically requeues due work with a fresh outbox event. |
| Attempt fencing | Worker and attempt identity plus a live lease protect terminal updates from stale owners. |
| Attempt-specific image candidates | Retries do not overwrite another attempt's output. Only the saved winning reference is downloadable. |

See the [architecture and lifecycle](docs/architecture.md) for transaction boundaries, failure windows and diagrams. [V5 ownership](docs/v5-safe-ownership.md) and [recovery validation](docs/v5-recovery.md) contain the detailed reliability evidence.

## Quick start · Windows

Prerequisites: **Git**, **Go matching [go.mod](go.mod)** (currently 1.27), **Docker Desktop with Compose and its Linux backend**, and **PowerShell 7 or Windows PowerShell 5.1**. Node.js is needed only for developer syntax checks, not application startup.

```powershell
git clone https://github.com/6kCraze/cloudforge.git
cd cloudforge
.\scripts\dev.ps1 setup -PullImages
.\scripts\dev.ps1 start
```

First setup creates a missing `.env` with generated local credentials, starts PostgreSQL/RabbitMQ and applies migrations. **For an existing database, review pending migrations and back up before `setup` or `migrate`.** Existing configuration is never overwritten; `start` never migrates. On subsequent runs, use `dev.ps1 infra` then `dev.ps1 start` to reuse cached images.

Open **[localhost:8080](http://localhost:8080/)** → **Launch workspace** → **Choose images**. Select the three small [included demo images](samples/README.md), then **Process 3 images**. Small jobs can finish between polls; the interface does not invent a visible running interval. Expand **Latest attempt details**, download a PNG, and reload the saved batch link.

The foreground launcher runs the API, dispatcher and **one worker** as local Go processes. Only PostgreSQL and RabbitMQ use Docker. No frontend build, Node server, application image build, or graphics framework is required.

```powershell
# In another terminal, inspect actual service readiness and broker counters:
.\scripts\dev.ps1 status
```

To stop: press **Ctrl+C once** in the foreground start terminal, let the Go processes finish, then run:

```powershell
.\scripts\dev.ps1 stop
```

This preserves named volumes, database rows and uploaded files. It refuses to stop infrastructure while the application is active. Do not remove volumes as routine cleanup.

See [setup, configuration and troubleshooting](docs/development.md). API and workers must share the same absolute `STORAGE_ROOT`; blank uses the per-user `CloudForge/storage` cache. The broker UI is [localhost:15672](http://localhost:15672/), with credentials from your own `.env`. Never commit private configuration or uploads.

Limits: **four files, 5 MiB each, 21 MiB per batch request, 8 megapixels and 8,192 pixels per side**. Thumbnails fit within **320 × 320** without upscaling. Storage has a conservative 256 MiB local admission budget. [API and storage details](docs/v4-image-workflow.md).

## Testing and evidence

```powershell
.\scripts\test.ps1                       # formatting, JS syntax, script safety, build, Go tests and vet
.\scripts\setup-test-infrastructure.ps1  # existing containers; restricted test database/account/vhost
.\scripts\test.ps1 -Integration          # sequential real database/messaging/image/recovery checks
```

Integration tests refuse development targets and use `cloudforge_test`, a restricted role/vhost, and temporary storage. Stop application processes before shared integration tests. Deterministic two-worker processors prove concurrency without CPU stress. Heavy real process-crash and broker-interruption cases are explicit opt-in checks; they are not repeated just for screenshots.

Local checks have passed. Real browser evidence includes upload → outbox → RabbitMQ → worker → PNG download, saved-batch reload, and controlled retry/failure presentation. **A completed green hosted run for the reviewed commit is required before release**; inspect the [actual GitHub Actions results](https://github.com/6kCraze/cloudforge/actions?query=branch%3Areview%2Fv6-portfolio). See [test isolation](docs/integration-testing.md), [CI coverage](docs/ci.md), [tooling validation](docs/v6-engineering-validation.md), and [portfolio validation](docs/v6-portfolio-validation.md).

## Screenshots and a short demo

[Desktop workspace](docs/images/workspace.jpg) · [Worker/attempt details](docs/images/job-details.jpg) · [Mobile](docs/images/mobile.jpg) · [Controlled retry/failure evidence](docs/images/retry-test-evidence.jpg).

All are authentic local browser captures. The retry screenshot uses deterministic isolated test faults, not production telemetry or a development crash. [Sample artwork and credits](samples/README.md) are included. [Recording steps and a 60–90 second narration](docs/demo.md) make the demo reproducible; no recording is claimed.

## Boundaries

- Single-machine local deployment and shared filesystem; no authentication or cloud deployment.
- At-least-once delivery, not exactly-once execution. Duplicate CPU work is possible; fenced commits protect the accepted result.
- Input/candidate orphans are retained; automatic storage reconciliation and garbage collection are not implemented.
- One dispatcher is supported. Malformed events are retained and logged; a full oldest batch of malformed events can still starve later rows.
- The UI exposes the latest saved attempt, not a complete event history or measured broker movements.
- Cancellation, additional image operations, Redis, S3/MinIO, packaging and tracing are optional backlog.

See [architecture limits](docs/architecture.md#observable-facts-and-honest-limits), the [roadmap](docs/roadmap.md), and the [historical engineering milestones](docs/engineering-history.md). The original V1 stuck-running crash demonstration remains documented in the history as the observed problem V5 recovery solves.
