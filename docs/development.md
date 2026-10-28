# Local developer workflow

CloudForge keeps PostgreSQL and RabbitMQ in Docker and runs the API, dispatcher, and normally one worker as local Go processes. No application container build or external infrastructure is needed.

## Requirements

- Git and a Go toolchain compatible with `go.mod` (currently Go 1.27.0).
- Docker Desktop with Linux containers, Compose v2, and its engine started.
- PowerShell 7 or Windows PowerShell 5.1. Commands resolve paths from their script directory, so your Windows username and working directory are not embedded in the tools.
- Node.js for JavaScript syntax checks only. CI uses Node 24; local checks also pass with Node 22. The browser application itself needs no Node runtime.

## First run

From the repository, run `./scripts/dev.ps1 setup -PullImages`, then `./scripts/dev.ps1 start`. The explicit PullImages switch downloads only PostgreSQL 17 and RabbitMQ 4 management images. Without it, the scripts use cached images or explain which image must be downloaded. Necessary Go modules may be downloaded by Go on a new machine; no optional browser or graphics packages are installed.

Setup creates a missing `.env` with generated local database/broker passwords and matching URLs, starts the two infrastructure services, waits for their healthchecks, and runs the existing Goose migrations. It does not overwrite `.env`, reset a database, reverse migrations, or delete volumes. Inspect and back up existing databases before an upgrade. Migrations are deliberately separate from ordinary application startup.

`start` builds the three binaries sequentially into ignored `.cloudforge/`, runs them in one foreground console, waits for `/readyz`, and prints the browser URL. The dispatcher publishes the outbox and runs recovery; one worker consumes job notifications. Keep that terminal open. Press Ctrl+C once to use the existing Go graceful-shutdown handlers. Then use `./scripts/dev.ps1 stop` to stop infrastructure. A bounded fallback terminates only still-running children started by that launcher and warns that leases will recover unfinished jobs. No unowned process is killed.

Use `./scripts/dev.ps1 status` from a second terminal. Container status, owned PIDs, API database readiness, and RabbitMQ's actual queue counts are separate observations. An API readiness response does not prove that a worker is connected; the broker consumer listing establishes that. `stop` refuses to disconnect a still-running owned application, an active API, or remaining broker clients.

## Command reference

| Command | Result |
| --- | --- |
| `./scripts/dev.ps1 init` | Create only missing development configuration; preserve any existing file. |
| `./scripts/dev.ps1 setup [-PullImages]` | Initialize config, start healthy infrastructure, explicitly apply migrations. |
| `./scripts/dev.ps1 infra [-PullImages]` | Start/reuse the two services without migrating. |
| `./scripts/dev.ps1 migrate` | Apply existing migrations deliberately, with application stopped. |
| `./scripts/dev.ps1 start` | Build and run API, dispatcher, and one worker in this terminal. |
| `./scripts/dev.ps1 status` | Inspect readiness, owned processes, and broker queue/consumer state. |
| `./scripts/dev.ps1 stop` | Stop infrastructure after application shutdown; retain persistent data. |
| `./scripts/test.ps1` | Formatting, JavaScript syntax, PowerShell checks, Go build, unit tests, vet. |
| `./scripts/setup-test-infrastructure.ps1` | Provision/migrate guarded test targets; generate only a missing `.env.test`. |
| `./scripts/test.ps1 -Integration` | Sequential ordinary real integration coverage. |

The infra/migrate/start/status/stop actions accept `-EnvFile <path>` for deliberate local configuration overrides. `init` and `setup` use the repository `.env`. These developer actions restore the caller's configuration environment after finishing, including previously absent variables. All generated credentials, storage, and runtime files are ignored by Git. The environment reader supports literal `KEY=value` lines and matching optional quotes, rejects malformed/duplicate keys, and does not execute or interpolate configuration text. For Compose/Go consistency, use literal values rather than shell substitutions.

## Common configuration issues

- **Docker is unavailable or not Linux:** start Docker Desktop and select its Linux backend. The scripts do not change engine/WSL settings.
- **Image not cached:** use the explicit `infra -PullImages` command once. Subsequent starts use `--pull never`.
- **Port occupied:** stop the conflicting process or change the relevant published port and corresponding URL together in `.env`.
- **Credentials no longer match a persistent volume:** restore the original matching `.env` or follow a reviewed credential-change procedure. Do not replace the volume. Initial PostgreSQL/RabbitMQ environment credentials do not reinitialize existing data.
- **Missing tables/pending migrations:** stop application processes, review migrations/back up, then use `dev.ps1 migrate`.
- **Missing images on a different terminal:** API and workers must share the same absolute `STORAGE_ROOT`. Blank selects the current user's cache directory. Another OS account has a different default.
- **A session is already active:** use its terminal to stop it. The runtime manifest checks PID, executable, and process start time so a reused PID is not treated as owned.
- **Test target rejected:** use the exact local `cloudforge_test` account/database/vhost documented in `.env.test.example`. There is no development fallback.

The individual `load-env.ps1` and `load-test-env.ps1` helpers remain available. They change only the calling PowerShell process environment. Normal users do not need to edit PATH, execution policy, WSL settings, or Docker resource limits through these scripts.

## Reading application activity

Services emit existing structured JSON logs to the foreground console. Claims, starts, successes, processing failures, and permanent failures carry job ID, worker ID, attempt ID/count, and the saved outcome where available. Retry decisions come from PostgreSQL; recovery sweeps log recovered/requeued counts. No image contents, configured passwords, or fabricated rates are added to logs. Use the job UUID to connect a log event with its saved API record; logs are operational evidence, not canonical state.

See [the system experience](v6-system-experience.md) for the browser and [CI](ci.md) for remote checks. Deployment packaging, release screenshots, and the final portfolio demonstration belong to checkpoint 3.
