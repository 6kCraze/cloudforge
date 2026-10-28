# Reproducible product demonstration

## Preparation

The connected browser tools expose screenshots, upload/download and viewport control, but **no screen-recording capability**. No video was created. Record the real local browser yourself with a recorder already available on your PC; the steps below need no CloudForge recording dependency.

1. Follow the [README quick start](../README.md#quick-start--windows). From the repository root run `./scripts/dev.ps1 infra`, then `./scripts/dev.ps1 start`. For a first installation use the documented `setup -PullImages` first. Review/back up an existing database before migration.
2. Keep exactly one worker. Open `http://localhost:8080/` in a browser at roughly 1440 × 1000. Close unrelated tabs/notifications. Do not show the terminal, `.env`, broker login, filesystem dialogs with private paths, or browser autofill in the recording.
3. Find the three included images in `samples/`: `alpine-lake.png`, `desert-dusk.png`, `orbit-study.png`. Combined inputs are about 10 KiB. The last tests transparency.
4. Start your recorder's browser-window capture. Hide/pause the recording while selecting files if the chooser exposes a local username. Resume before submitting. Do not alter application screenshots or job fields.

## Shot list and narration · approximately 75 seconds

| Time | Actual action | Narration |
| --- | --- | --- |
| 0–12 s | Show the city; select **02 State**. | “CloudForge is a Go background processing platform. This city explains the components. PostgreSQL owns job state; the routes are illustrative, not live traffic.” |
| 12–28 s | Select **Launch workspace**, **Choose images**, all three sample PNGs, then **Process 3 images**. | “A batch becomes durable jobs and outbox events in one database transaction. The dispatcher publishes confirmed work notifications to RabbitMQ.” |
| 28–42 s | Observe the returned states and previews. Small jobs may already be finished. | “Workers claim jobs atomically and generate thumbnails. These counts are saved database states. Small jobs can finish between polls, so I’m not simulating a slower running state.” |
| 42–55 s | Expand **Latest attempt details** on one card. | “The card shows the actual worker and attempt. Leases and heartbeats track ownership. A stale attempt cannot commit a newer owner’s result.” |
| 55–65 s | Select **Download PNG**, open the downloaded image, then reload the saved batch URL. | “The result is a real PNG, and this batch remains accessible after reload. Only the accepted attempt’s output is served.” |
| 65–80 s | Return to the city or show the architecture diagram. | “If a worker crashes, the dispatcher recovers its expired lease. Due retries are requeued with a fresh outbox event in one transaction. Attempts are bounded. Recovery and fencing are backed by isolated fault tests; this demonstration does not crash development services.” |

Do not delay processing to reach the suggested duration. It is fine to finish sooner. If including [retry/failure test evidence](images/retry-test-evidence.jpg), explicitly say **“isolated controlled test, not live production monitoring.”** The screenshot shows genuine persisted states injected by the opt-in fixture; it is not evidence of a fresh killed-worker test.

## Reproduce controlled UI evidence separately

Stop the normal application before shared integration work. With existing infrastructure running and isolated test infrastructure already provisioned:

```powershell
. .\scripts\load-test-env.ps1
$env:CLOUDFORGE_BROWSER_DEMO = '1'
$env:CLOUDFORGE_BROWSER_STOP_FILE = Join-Path $env:TEMP ('cloudforge-browser-' + [guid]::NewGuid().ToString('N') + '.signal')
Write-Output $env:CLOUDFORGE_BROWSER_STOP_FILE
$env:GOMAXPROCS = '2'
go test -p 1 -tags=integration ./internal/system -run '^TestV6BrowserSession$' -count=1 -v -timeout=23m
```

Open `http://localhost:18080/`. The test prints its fixture directory. Select only `retry.png` and `failure.png` there. The first genuinely records `retry_wait`, waits a configured **12 seconds in this test only**, then succeeds on attempt two. The second records a permanent failure. One real worker, API, outbox dispatcher and recovery loop run against the guarded test database/vhost; no development rows are used. Never point this fixture at development targets.

The other `running.png` fixture blocks until the printed `RELEASE_RUNNING` file exists; it is optional and unnecessary for the normal product demo. Do not upload it accidentally. In another terminal, stop the session gracefully by creating the exact stop-file path printed above:

```powershell
Set-Content -LiteralPath '<the stop-file path printed in the first terminal>' -Value 'stop'
```

This ends the opt-in test and its isolated fixture cleanup. The temporary batch URLs in the screenshots are evidence identifiers, not permanent public demos. Remove the three opt-in environment variables from that terminal afterward, or close it. Do not delete persistent development volumes or run down migrations.

## Shutdown

After normal recording, press Ctrl+C once in the foreground launcher. Wait for API, dispatcher/recovery and worker shutdown, then run `./scripts/dev.ps1 stop`. Preserve uploads and volumes. Never kill the development database or workers for cinematic effect.
