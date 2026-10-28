# V6 checkpoint 2 validation

Validated locally on October 27, 2026. V5 remains released at `286370d` / `v0.5.0`; both V6 checkpoints remain uncommitted. No new migration, deployment service, application container, or processing-lifecycle change was introduced.

## Results

| Check | Observed result |
| --- | --- |
| Ordinary checks | `scripts/test.ps1` completed Go build, ordinary tests, vet, JavaScript syntax, formatting, and tooling safety checks. Windows CRLF-only differences are normalized for formatting comparison. |
| Integration | `scripts/test.ps1 -Integration` passed database, jobs, dispatcher, worker, and system packages sequentially, plus integration vet. Real outbox/publication/ACK/duplicate, two-worker, image, lease, retry, and recovery cases remain covered. |
| Workflow validation | A checksum-verified standalone actionlint 1.7.12 executable accepted `ci.yml`, including expressions and action inputs. Action SHA pins were verified against official v7 tags. |
| Remote GitHub Actions | **Not executed.** Workflow is uncommitted/unpushed. A successful hosted run remains a release gate. |
| PowerShell | Syntax and helper behavior passed under PowerShell 7 and Windows PowerShell 5.1. Checks include missing/duplicate configuration, generated matching credentials, no-overwrite initialization, quoted paths, unsafe test URLs, process identity JSON roundtrips, stale identity rejection, and environment removal/restoration. |
| Setup | `dev.ps1 setup` reused existing cached services and preserved `.env`; Goose reported no pending migrations, version 7. No down/reset operation ran. |
| App launch | Foreground launcher built/started API, dispatcher, and exactly one worker. Isolated smoke configuration used port 18081, restricted test database/vhost, and temporary storage. Status reported all three owned PIDs, ready database API, and one actual broker consumer. |
| Safe shutdown | Active-session `stop` was rejected. Ctrl+C produced API graceful-stop, dispatcher/recovery-stop, and worker-stop messages without the forced fallback. Infrastructure then stopped safely. Repeated stop and stopped-status succeeded under Windows PowerShell 5.1. |
| Browser compatibility | A real 120×80 PNG upload reached succeeded through the running three-process app. Preview loaded, browser download decoded as PNG, and the saved batch reloaded. No browser console errors were reported. |
| Focus regression | Details summary was focused while the returned job was queued. It stayed focused for the same UUID after the polling response rebuilt the succeeded card. |
| Fresh local clone | An actual local Git clone plus the pending V6 working-tree overlay contained no copied private `.env` files. `dev.ps1 init` generated configuration, repeated init preserved it, and `go build -p 1 ./...` passed with dependency downloads disabled. This is not a claim of a full new-machine install or brand-new infrastructure test. |

The browser smoke batch was `21ffe58c-dcf2-4062-8bf5-9ad7ebd7ed64`; its job `7b25a09e-78e0-4612-be30-6218186ce33a` succeeded under `v6-dx-smoke-worker` on attempt 1. The attempt UUID in structured claim/start/completion logs matched the persisted record. This is functional evidence, not a throughput benchmark. Smoke data lives only in the guarded test namespace and temporary storage.

## Focused frontend review

- Consolidated duplicate header and media-query rules without redesigning the city/workspace.
- Preserved keyboard focus and expanded details when refreshed job cards are replaced.
- Preserved selected-file object URLs for a bfcache page, and resumed incomplete polling on restoration. Actual bfcache admission/restoration was not forced in this session; that branch was source-reviewed. Ordinary upload, polling, download and reload were executed.
- Confirmed text content is built with DOM/textContent APIs rather than untrusted HTML insertion. Existing polling remains bounded, cancels superseded requests, and stops at terminal/paused states. Scene animation remains event-driven, offscreen-paused, and reduced-motion aware. Global listeners are installed once.
- Checkpoint 1's full responsive/motion validation was retained; the focused smoke check introduced no horizontal overflow at the browser's current desktop viewport. A large repeat of the prior visual/fault suite was intentionally not run.

## Preservation and resources

Development remained at migration 7, with 16 jobs, 16 outbox events, four batches, and six attempts. The original development snapshot, all four V3 demonstration rows, and original output references matched the saved snapshot exactly. All ten original stored-file SHA-256 values matched. Existing `.env` and `.env.test` hashes were unchanged. The PostgreSQL/RabbitMQ named volumes remained present.

Only the two existing containers were used. Normal smoke used one worker; deterministic integration cases used at most two. No local image pulls, Docker builds, browser downloads, CPU stress tests, WSL/Docker setting changes, or new application dependencies were used. The small workflow checker download was 2.5 MB and stayed outside the repository. Go application processes exited, infrastructure was stopped, and temporary preview tabs were closed. Automatic approval review blocked the scratch-file cleanup command with the reason "blocked by policy". The fresh-clone scratch checkout and two ignored temporary smoke configuration files were therefore retained, with no running processes. Persistent development data was not removed.

## Exact working-tree file manifest

There are **17 files touched for checkpoint 2**, plus **four checkpoint-1-only files retained**, for **21 total pending V6 files**.

| File | Scope |
| --- | --- |
| `.github/workflows/ci.yml` | CP2: one pinned workflow, sequential checks, generated CI credentials, opt-in faults. |
| `.gitignore` | CP2: ignore `.cloudforge/` runtime/build output. |
| `README.md` | CP1 + CP2: retained experience link; new reproducible setup/check commands. |
| `docs/development.md` | CP2: setup, storage, readiness, shutdown, troubleshooting. |
| `docs/ci.md` | CP2: CI coverage, isolation, manual faults, remote-result boundary. |
| `docs/integration-testing.md` | CP2: reuse bootstrap/runner, preserve isolation and configuration. |
| `docs/v6-engineering-validation.md` | CP2: this report and manifest. |
| `docs/v6-system-experience.md` | CP1 retained. |
| `internal/httpapi/web/app.css` | CP1 + CP2: cinematic visual foundation plus targeted duplicate-rule cleanup. |
| `internal/httpapi/web/app.js` | CP1 + CP2: original functional experience plus focus/bfcache repairs. |
| `internal/httpapi/web/icon.svg` | CP1 retained. |
| `internal/httpapi/web/index.html` | CP1 retained. |
| `internal/system/v6_browser_integration_test.go` | CP1 retained. |
| `internal/worker/worker.go` | CP2: structured attempt/state correlation fields only. |
| `scripts/common.ps1` | CP2: shared configuration, safety, process-identity helpers. |
| `scripts/dev.ps1` | CP2: init/setup/infra/migrate/start/status/stop. |
| `scripts/load-env.ps1` | CP2: reuse validated environment reader. |
| `scripts/load-test-env.ps1` | CP2: reuse reader and reject unsafe isolated targets. |
| `scripts/setup-test-infrastructure.ps1` | CP2: preserve/generate config and support ephemeral CI settings. |
| `scripts/test.ps1` | CP2: quality, normal integration, explicit fault checks. |
| `scripts/tooling-tests.ps1` | CP2: dependency-free PowerShell safety/syntax regressions. |

## Remaining release work

Checkpoint 3 should obtain a real green hosted CI result after approval to publish the changes, complete the final architecture/design explanation and portfolio README, select final screenshots/demo inputs, record a short reproducible demonstration, review known limitations/resume claims, and perform the final release validation for v0.6.0. Full setup on a new machine and real bfcache restoration remain unverified. No checkpoint 3 implementation or Git release action was performed here.
