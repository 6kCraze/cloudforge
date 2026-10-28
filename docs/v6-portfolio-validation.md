# V6 portfolio checkpoint and release gate

Local checkpoint snapshot from October 27–28, 2026, before publication of the V6 review branch. At that snapshot, the base release was `v0.5.0` at `286370d`, and the V6 work on `review/v6-portfolio` had no commit, push, tag, or hosted CI run. Current hosted outcomes are available in the [review branch runs](https://github.com/6kCraze/cloudforge/actions?query=branch%3Areview%2Fv6-portfolio); match the reviewed commit SHA.

## Deliverables

- [README](../README.md) now leads with the actual product, a genuine city capture, completed batch capture, architecture, engineering decisions, a short Windows startup path, real test scope and current limits. Historical phase details moved to [engineering history](engineering-history.md).
- [Architecture](architecture.md) and [GitHub-compatible SVG](images/architecture.svg) depict the actual API, PostgreSQL, dispatcher/recovery loop, RabbitMQ, workers, and local output store. They distinguish durable lifecycle state from broker delivery and show the due retry/outbox transaction.
- [Portfolio images](images/) are direct captures from the local browser: city, completed workspace, latest worker/attempt details, mobile, and a separately labeled controlled retry/failure test. [Included sample artwork](../samples/README.md) is original CC0 geometric PNG material; no private uploads were used. The five screenshot JPEGs are under 85 KiB each, combined under 400 KiB.
- [Demo guide](demo.md) has exact startup, browser actions, recording hygiene, narration and stop instructions. Browser capture tools offered screenshots but no video recording, so no video is claimed.
- A complete Job Journey was **deferred**. The existing API exposes the latest job fields; it does not expose all `job_attempts` rows or every intermediate transition. The current card keeps true latest-attempt fields without inventing timestamps or broker observations.
- The existing SVG city, responsive workspace, focus restoration and motion controls from checkpoints 1–2 remain intact. Checkpoint 3 made no production behavior or styling changes; visual review found no quality issue that justified risking the working UI.

## Local validation observed

| Check | Result and boundary |
| --- | --- |
| Ordinary quality | `scripts/test.ps1` passed formatting, JavaScript syntax, PowerShell safety checks, build, ordinary Go tests and vet. These checks are not hosted CI. |
| Real integration | Earlier checkpoint-2 `scripts/test.ps1 -Integration` passed database, jobs, dispatcher, worker and system packages sequentially. No production processing code changed in checkpoint 3, so the heavy suite was not repeated. |
| Opt-in browser integration | `TestV6BrowserSession` passed in the restricted test namespace with real PostgreSQL, RabbitMQ, API, dispatcher/recovery and one worker. Its 523-second lifetime includes human screenshot work, not benchmark performance. |
| Three-image product path | Batch `d8b60b51-c678-42be-bde7-b173144a540a` processed all three included 640 × 400 sample PNGs. API reported 3/3 succeeded, each with a 320 × 200 output URL and attempt UUID. The browser displayed previews and the saved batch reloaded. |
| Download | The browser downloaded the transparent sample’s output. A separate decoder verified a real 320 × 200 RGBA PNG with a nontrivial alpha range. |
| Retry/failure presentation | Test batch `f2d8e477-d67b-413f-8c03-6840bb075197` showed `retry_wait` with a saved due time and one permanent `failed` job. The retry later succeeded on attempt two. These were deterministic **isolated test faults**, not a killed development worker or production event. |
| Mobile and architecture | Browser at 390 px showed results, a preview and latest-attempt details with document width 375 px, so there was no horizontal overflow. Component `02 State` changed the selected building and explained PostgreSQL authority. Reduce-motion toggle changed its announced state. No browser warning/error logs were observed for the final test batch. |
| Diagram and files | The SVG rendered in the local browser with readable labels and arrows. Its accessible title/description identify the topology. Screenshots were visually inspected and kept as small JPEGs. |
| Development preservation | The saved 13-line original snapshot, including all four V3 demo job rows, matched read-only queries. All ten original stored-file sizes and SHA-256 hashes matched. Development stayed at Goose migration 7 with 16 jobs and 16 outbox rows; private `.env` and `.env.test` hashes were unchanged. |
| Runtime after verification | Temporary browser fixture and Python diagram preview exited. No CloudForge Go process or demo-port listener remained. Docker Desktop was manually paused when selective container shutdown was attempted; the existing named volumes remain untouched. |

The browser fixture's test database and temporary storage are **not** the development database. Earlier checkpoint-1 and checkpoint-2 evidence covers four-image upload, polling/focus, accessibility, actual three-process launcher behavior, safe Ctrl+C, restricted integration setup, two-worker concurrency, and V5 fault tests. Full clean-machine setup and actual browser bfcache restoration remain unverified. No formal throughput, uptime, or hosted deployment claim is made.

## Known limits and release gates

The application uses local filesystem storage and local processes, has no authentication, and retains orphan attempt files. Delivery and publication are at least once. A complete historical Job Journey needs an attempt-history API. A full oldest batch of malformed outbox events can still block later rows. The desktop experience and synthetic API coexist with these limitations; no unimplemented feature is presented as shipped.

**Hosted GitHub Actions was pending at this local checkpoint.** Before `v0.6.0`: review the complete intentional branch diff and assets, publish the approved review commit, inspect the actual hosted workflow (including normal integration; manual fault option remains separate), repair any real failure, then review the final diff and authorize merge/release. Do not label local workflow syntax validation as remote CI success.

Scratch files blocked from deletion in checkpoint 2 remain outside deliverables: a temporary clean clone under the user temp directory, two ignored `.env.runtime-smoke-*` files in this checkout, and the standalone workflow checker under temp. No new attempt was made to remove those blocked files. The checkout ignores private env files, `.cloudforge/`, user storage and binaries.

## Exact review inventory

The prepublication `git status --short --untracked-files=all` listed **37 intended, unstaged files**. None were `.env` secrets, generated binaries, uploaded user images or database dumps. The repo scans found only generated CI password variables, documented local placeholder URLs and isolated test strings; no plaintext private credential was found. `git diff --check` reported no whitespace error. The active branch was `review/v6-portfolio`; `main` and the `v0.5.0` tag pointed to `286370d`.

| Scope | Files |
| --- | --- |
| Site and worker | `internal/httpapi/web/app.css`, `internal/httpapi/web/app.js`, `internal/httpapi/web/icon.svg`, `internal/httpapi/web/index.html`, `internal/worker/worker.go` |
| Development and test tooling | `.gitignore`, `.github/workflows/ci.yml`, `internal/system/v6_browser_integration_test.go`, `scripts/common.ps1`, `scripts/dev.ps1`, `scripts/load-env.ps1`, `scripts/load-test-env.ps1`, `scripts/setup-test-infrastructure.ps1`, `scripts/test.ps1`, `scripts/tooling-tests.ps1` |
| Documentation | `README.md`, `docs/architecture.md`, `docs/ci.md`, `docs/demo.md`, `docs/development.md`, `docs/engineering-history.md`, `docs/integration-testing.md`, `docs/roadmap.md`, `docs/v6-engineering-validation.md`, `docs/v6-portfolio-validation.md`, `docs/v6-system-experience.md` |
| Diagram and actual browser captures | `docs/images/architecture.svg`, `docs/images/overview.jpg`, `docs/images/workspace.jpg`, `docs/images/job-details.jpg`, `docs/images/mobile.jpg`, `docs/images/retry-test-evidence.jpg` |
| Public demo assets | `samples/README.md`, `samples/alpine-lake.png`, `samples/desert-dusk.png`, `samples/orbit-study.png`, `samples/generate.py` |

These are the full V6 checkpoint-1 through checkpoint-3 changes reviewed for publication. The subsequent resume request authorized a review commit and branch push; merge and `v0.6.0` tagging still require separate release approval.

## Resume verification on October 28, 2026

- Fresh Git checks confirmed exactly 37 pending files on `review/v6-portfolio`, with HEAD, local/remote `main`, and the peeled annotated `v0.5.0` tag at `286370d39b04139b5277a9dcf4f531ff3fbc5275`. No review branch or V6 tag existed remotely at the start. GitHub reported the repository as private.
- A fresh `scripts/test.ps1` run passed formatting, JavaScript syntax, PowerShell safety, Go build, ordinary Go tests and vet. The tooling checks also passed separately under Windows PowerShell 5.1. Cached Go test results are identified as cached in command output.
- Visual and format inspection found five JPEG screenshots named `.png`. They were renamed to `.jpg`, their links and inventory corrected, and all five SHA-256 values verified unchanged. The original demo inputs and generated thumbnail outputs remain PNGs.
- All intended files were individually reviewed. No private-key/token pattern or machine-specific user path was found. Credential URL matches were generated CI variables, template replacements, or invalid test fixtures. Action SHA pins matched the official v7 tags. Runtime configuration, scratch material, user storage and binaries remained excluded.
- Historical browser captures and the saved successful batch JSON were inspected under the sibling `cloudforge-release-evidence` directory. These are prior evidence, not a newly executed browser session. This review did not start local infrastructure, change development rows, or repeat heavy fault tests; hosted integration validates the review commit separately.
- The V6 quick start explicitly switches a fresh clone to `review/v6-portfolio` before invoking the launcher, because the preserved V5 `main` does not contain that script. Release preparation should remove that temporary branch-selection step when V6 is merged.

## Resume wording grounded in the code and tests

- Built CloudForge, a Go image-processing application that creates durable PostgreSQL jobs and outbox events, delivers work through RabbitMQ, and displays batch progress and downloadable thumbnails in a responsive browser UI.
- Implemented lease-based worker ownership, heartbeats, bounded retries and attempt fencing so abandoned jobs can be redispatched and stale workers cannot commit another attempt’s result.
- Validated transactional publication, manual acknowledgements, duplicate claims, two-worker concurrency and crash recovery with isolated PostgreSQL/RabbitMQ integration and fault tests.
