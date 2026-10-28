# V6 checkpoint 1: the city behind every job

CloudForge's Go-served browser now combines a cinematic overview, an explorable infrastructure city, and the image workspace. The architecture, state transitions, migrations, and processing services are unchanged.

## Visual implementation

The city is an original inline SVG: six architectural silhouettes, faceted roofs, warm windows, a raised ground plane, and cyan routes. No remote fonts, raster illustration, video, frontend packages, or build system are needed. The four browser assets total approximately 92 KiB before compression. The SVG contains 324 polygons.

The scene stays pinned alongside the desktop explanation. Scroll events schedule at most one pending animation frame; transforms move the scene and headline reversibly. IntersectionObserver pauses the decorative route animation offscreen. Component buttons work with keyboard focus and Enter/Space; the buildings also support pointer selection. The same component descriptions remain available without relying on the miniature map labels.

At tablet/mobile sizes the scene becomes a normal document section. Mobile route animation and camera motion are disabled. The operating system's `prefers-reduced-motion` setting selects static motion rules. A visible Reduce motion control also turns off route animation, transitions, camera transforms, and smooth scrolling for the current page session. Without JavaScript, the scene and explanation remain readable, with a notice that the workspace requires JavaScript.

## Data contract

| Indicator | Evidence and meaning |
| --- | --- |
| Intake highlight | A successful batch POST response received by this page; not an in-flight upload or service-health signal. |
| Durable-state highlight | The selected batch's returned job-state counts. A queued job does not establish broker delivery. |
| Worker highlight | Returned running, retry_wait, or failed job states. It does not establish worker health or the number of live workers. |
| Output highlight | At least one job has saved succeeded state. Missing/corrupt output still produces a preview or download error. |
| Snapshot timestamp | When this browser retrieved the response, not when PostgreSQL committed it. Paused/error states explicitly retain the last snapshot. |
| Dispatcher, RabbitMQ, routes, and scroll emphasis | Conceptual responsibilities and connections. No queue depths, publication events, traffic rates, transfer animation telemetry, or throughput is inferred. |

Only the existing batch API is used. Result cards show the current job's attempt count, available worker identity, latest attempt ID, start time, retry time, and the duration between saved start/completion timestamps. Full attempt-history rows are not exposed by the existing API, so the UI labels this **Latest attempt details** and does not fabricate a timeline. An unassigned current job is not presented as having a known previous worker.

Failure codes map to useful, bounded descriptions; raw processor errors, filesystem paths, and stack traces are not rendered. Upload rejection, unknown batch, connection timeout, paused updates, and unavailable preview states remain distinct. Failed batch loading has a visible retry action and never reuses another batch's completion state.

## Validation performed on October 27, 2026

- `node --check internal/httpapi/web/app.js` passed.
- `go test -p 1 ./internal/httpapi` passed after the final frontend changes.
- `go vet -p 1 ./internal/httpapi` passed.
- `go vet -p 1 -tags=integration ./internal/system` passed, including the final browser-fixture cleanup change. `git diff --check` passed.
- The existing isolated `TestV4BrowserSession` passed after a real JPEG upload and a four-image JPEG/PNG batch. All five jobs succeeded, all previews loaded, all five browser downloads decoded as PNG, and the completed batch reloaded.
- The new isolated `TestV6BrowserSession` exercised real persisted queued, running, retry_wait, succeeded, and failed states. One small processor blocks on a test-only release file, one deliberately fails its first attempt, and one fails permanently. The real recovery loop/outbox/worker completed the recoverable job on attempt 2. Its PNG downloaded on mobile. These are fixture-injected faults, not production debug controls.
- The final rebuilt frontend also completed another small PNG upload, preview, mobile download, and saved-batch reload. All seven downloaded PNGs across the browser sessions were decoded locally with their expected dimensions.
- Desktop 1440×1000, tablet 768×1024, and mobile 390×844 had no horizontal overflow. Mobile results use one readable column. All six component descriptions, keyboard selection, visible focus, invalid-file rejection, the four-file limit, unavailable-batch retry, and pause/resume were exercised.
- Desktop scrolling pinned the scene at 110px, changed the camera transform, and reversed it when scrolling back. Offscreen routes reported paused animation. The Reduce motion control reported no route animation and no camera transform, with automatic scrolling. OS preference handling is implemented in CSS/matchMedia; changing or emulating the OS preference was not performed in this browser session.
- No JavaScript or CSP errors were reported in the final successful workflow. Deliberate unknown-batch requests returned the expected 404 response.
- A palette contrast check returned 6.95:1 for secondary panel text, 5.37:1 for map coordinates, 5.33:1 for failure status, 7.76:1 for retry status, and 7.84:1 for running status. This is a focused palette check, not a full accessibility certification.

Screenshots, downloaded-file evidence, and API snapshots are stored outside the repository in the local `cloudforge-release-evidence/v6` directory. These tests used the guarded `cloudforge_test` database/vhost and temporary image storage, with one worker. No stress workload, additional service, Docker build, or image pull was used. No new performance or benchmark claim is made.

The development database remained at migration 7, with 16 jobs, 16 outbox events, four batches, ten batch-image rows, and six attempts. The saved original-development snapshot (including all four V3 demonstration jobs and original output references) matched exactly. The ten original storage files retained their saved SHA-256 hashes. Existing PostgreSQL and RabbitMQ named volumes were preserved.

Browser-fixture sessions closed successfully, and PostgreSQL and RabbitMQ were stopped after validation. Screenshots remain available without leaving the application runtime running.

## Repeating a focused browser check

Start the existing infrastructure with `docker compose up -d --wait --pull never db rabbitmq`. Use the existing isolated-test setup from [integration testing](integration-testing.md); never substitute development URLs. From PowerShell:

```powershell
. .\scripts\load-test-env.ps1
$env:CLOUDFORGE_BROWSER_DEMO = '1'
$env:CLOUDFORGE_BROWSER_STOP_FILE = Join-Path $env:TEMP ('cloudforge-browser-stop-' + [guid]::NewGuid() + '.signal')
go test -p 1 -tags=integration ./internal/system -run '^TestV6BrowserSession$' -count=1 -v -timeout=23m
```

The test prints the browser URL, temporary fixture directory, and release-file path. Open the URL, upload the printed fixtures, and create the printed release file from another terminal to let `running.png` finish. `retry.png` waits 12 seconds after its deliberately failed first attempt; `failure.png` fails permanently. Create the chosen stop file to close the session gracefully. The fixture has a 20-minute ceiling, releases its blocked processor on cleanup, and cleans only its guarded test namespace. Stop the existing infrastructure afterward with `docker compose stop db rabbitmq`; preserve volumes.

## Boundaries and review

The scene is a curated six-component architecture, not a broker monitoring console. It cannot report dispatcher publication or a live worker inventory. Full attempt history needs a separate reviewed read-only API before it can appear. The local motion toggle resets on reload; the system preference is always honored. The miniature map labels are secondary to the accessible component controls.

The next visual refinement is portfolio photography and final screenshot composition during the documentation/demo checkpoint. Checkpoint 2 can focus on developer setup and CI. This checkpoint does not introduce deployment packaging or a new public version tag.
