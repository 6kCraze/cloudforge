# V5 reliability: ownership, recovery, and fault validation

Checkpoint 2 builds on [safe worker ownership](v5-safe-ownership.md); checkpoint 3 adds real failure validation. Migrations `00006` and `00007` were first validated in the isolated `cloudforge_test` database. After separate approval, the development database was backed up and upgraded from version 5 to 7. Older V4 application processes must be stopped before this upgrade.

## Lifecycle and decisions

`queued` jobs become `running` only through a successful PostgreSQL claim. Claims alone increment `attempt_count` and create a fresh UUID attempt. A recoverable processing error moves a valid attempt to `retry_wait` with a PostgreSQL-clock `next_retry_at`. The first and second failures wait 1 and 2 seconds by default. A third failed attempt becomes terminal `failed`. Retry base and cap are configurable with `RETRY_BASE_DELAY` and `RETRY_MAX_DELAY`; the default cap is 4 seconds. The total processing-attempt limit is three.

Malformed or unsupported images, invalid job payloads, and a permanently missing required input fail on their current attempt. Other processing errors are treated as recoverable within the bound. Database errors before claim do not consume an attempt. A worker acknowledges a delivery only after the database confirms success, permanent failure, or a durable retry decision. An uncertain database result follows the existing NACK/reconnect path.

PostgreSQL retains each attempt's worker, timing, outcome, error, and retry decision. The current job row exposes `retry_wait` and `next_retry_at`; the batch API reports `retry_wait` separately, and the browser says “Waiting to retry.” Completed V4 results and the synthetic SHA-256 job type remain supported.

## Recovery and redispatch

The existing dispatcher process runs a separate lightweight recovery loop every 500 ms by default (`RECOVERY_POLL_INTERVAL`). Each database operation has a five-second deadline and selects at most 16 rows with `FOR UPDATE SKIP LOCKED`. It runs independently of RabbitMQ connectivity.

One short transaction finds expired `running` leases, records the abandoned attempt as `expired`, and either schedules `retry_wait` or marks the third attempt `failed`. Another finds due `retry_wait` jobs and atomically changes each to `queued` **and** inserts a fresh `job.ready` outbox event. The dispatcher publishes that event with its existing confirms and mandatory-return handling, outside the database transaction. If the broker is unavailable, the unpublished event remains in PostgreSQL.

Outbox event IDs remain unique primary keys. Migration `00007` replaces one-ready-event-per-job uniqueness with one event per `(event_type, job_id, delivery_generation)`: generation 0 is initial creation; generations 1 and 2 follow the first two failed attempts. Row locks and status predicates prevent duplicate recovery transitions. The generation constraint catches accidental duplicate event insertion for one transition. A duplicate broker delivery during `retry_wait` is acknowledged without execution; once due, only `queued` jobs can be claimed.

## Outputs and remaining work

Every image attempt still writes an exclusive `outputs/<job_id>/<attempt_id>/thumbnail.png` candidate. A stale attempt cannot update the succeeded PostgreSQL result, and the download route opens only the winning attempt's file. V4 results without an attempt ID retain their legacy path. Files from failed or uncertain attempts are retained. Reconciliation should compare storage paths with PostgreSQL job results and attempt history before proposing any deletion; no automatic cleanup is present.

The migration down path refuses to discard active `retry_wait` rows. Reinstating the older outbox uniqueness also fails safely if a job already has multiple ready events. No development rollback was run.

## Checkpoint 3 fault evidence

The isolated integration suite starts an actual separate Go worker executable for `TestV5RealWorkerProcessCrashRecoversImage`. In one case the worker blocks immediately after claiming an image; in another it finishes an attempt-specific PNG candidate and blocks before recording PostgreSQL success. The test observes the attempt UUID, live lease, and heartbeat, kills only that test-owned process, consumes the old RabbitMQ redelivery, and waits for the lease to expire naturally. Recovery records the expired attempt, schedules and releases the retry, and creates a fresh unpublished outbox event. A replacement Go worker completes the image in a second attempt; the download returns the winning PNG. Both cases use small 3×2 images and bounded timeouts.

`TestV5LiveStaleImageAttemptCannotReplaceWinner` keeps an old processor alive after its lease expires in the isolated database. Its renewal is rejected, worker B succeeds with a different attempt, and the late old completion is rejected. The old candidate cannot replace worker B's file, and the download endpoint resolves through PostgreSQL's winning result.

After checking that the shared RabbitMQ broker had no other connections or consumers, the opt-in `TestV5LiveBrokerInterruptionRetainsRecoveryIntent` stopped and restarted the existing broker container without removing its volume. PostgreSQL recovered and queued a job during the outage; its outbox row remained unpublished. The dispatcher reconnected and published after restart, and a worker completed and acknowledged the job. The test requires `CLOUDFORGE_LIVE_BROKER_STOP=1` and should not run while unrelated local clients use the shared broker. Ordinary integration runs skip it.

To repeat that test, first inspect both vhosts and any local API/dispatcher/worker processes, then load `.env.test` and run `go test -p 1 -tags=integration ./internal/system -run '^TestV5LiveBrokerInterruptionRetainsRecoveryIntent$' -count=1 -timeout=55s` with `CLOUDFORGE_LIVE_BROKER_STOP=1` set only for that command. Never enable it in a broad test suite.

The existing retry tests cover due-time delays, three total attempts, terminal exhaustion, permanent failure history, duplicate recovery scans, and batch `retry_wait` views. V4 single-image, four-image batch, legacy output, and synthetic-job regressions remain in their respective test files. RabbitMQ publication is **at least once**: a confirmed publish followed by a failed PostgreSQL `published_at` update can produce a duplicate delivery; the conditional job claim prevents duplicate processing.

## Development upgrade and rollback record

The development upgrade was approved and applied after these checks. For another database, stop older API, dispatcher, and worker processes before running V5 binaries. From the repository root, load `.env`, verify the URL target, and confirm version 5. These commands prepare evidence and backups; they do not migrate:

```powershell
. .\scripts\load-env.ps1
$target = [Uri]$env:DATABASE_URL
if ($target.Host -notin @('localhost', '127.0.0.1') -or $target.AbsolutePath -ne '/cloudforge') {
    throw 'Expected the local development cloudforge database'
}
docker compose up -d --wait --pull never db rabbitmq
go run ./cmd/migrate status
docker compose exec -T db psql -U $env:POSTGRES_USER -d $env:POSTGRES_DB -c "SELECT id, status, attempt_count, claimed_by, started_at, completed_at FROM cloudforge.jobs WHERE claimed_by IN ('worker-01','worker-02') ORDER BY id;"
docker compose exec -T db psql -U $env:POSTGRES_USER -d $env:POSTGRES_DB -c 'SELECT count(*) FROM cloudforge.jobs; SELECT count(*) FROM cloudforge.outbox_events; SELECT count(*) FROM cloudforge.batches;'
$backupDir = Join-Path $env:USERPROFILE ('Documents\CloudForgeBackups\pre-v5-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Path $backupDir -Force | Out-Null
docker compose exec -T db pg_dump -U $env:POSTGRES_USER -d $env:POSTGRES_DB -Fc -f /tmp/cloudforge-v5-preupgrade.dump
docker compose exec -T db pg_restore --list /tmp/cloudforge-v5-preupgrade.dump
$backupFile = Join-Path $backupDir 'cloudforge-v5-preupgrade.dump'
docker compose cp db:/tmp/cloudforge-v5-preupgrade.dump $backupFile
Get-FileHash -LiteralPath $backupFile -Algorithm SHA256
$storageRoot = if ($env:STORAGE_ROOT) { $env:STORAGE_ROOT } else { Join-Path $env:LOCALAPPDATA 'CloudForge\storage' }
Copy-Item -LiteralPath $storageRoot -Destination (Join-Path $backupDir 'storage') -Recurse
```

Review the `status` output for migration 5 and no dirty state; verify that the four demonstration rows and counts match the saved V4 evidence, that the archive copied successfully, and that the storage copy is complete. A readable archive is not a verified full restore. Preserve the backup directory. If any check fails, stop before upgrading.

After approval and successful checks, run `go run ./cmd/migrate up` with the same development environment. Goose applies `00006` then `00007`; do not point this command at the test URL. Check `go run ./cmd/migrate status` for version 7, then repeat the read-only record queries and compare them with the snapshot. Confirm the new `job_attempts` table and `outbox_events.delivery_generation` column, then start one V5 API, dispatcher, and worker. Existing `running` V4 jobs receive expired leases in migration 6 and can be recovered by the V5 dispatcher; decide deliberately whether that behavior is appropriate for each such job before starting it. In this development upgrade, all 10 existing jobs were already terminal, so no old running attempt required backfill or recovery.

The approved development run verified both the older archive and a fresh pre-V5 custom-format archive with `pg_restore --list`; the fresh archive was copied outside Git with matching SHA-256 hashes. This was an archive listing and copy check, **not** a full restore test. The database advanced from version 5 to 7 in order. The original 10 jobs, 10 outbox events, two batches, five batch-image rows, four V3 demonstration rows, output references, and 10 storage files matched their pre-upgrade snapshots after migration and after the smoke test.

One local API, dispatcher, and worker then processed one PNG and a four-PNG batch through the browser. All five new image jobs succeeded with distinct attempt UUIDs and succeeded attempt-history rows; previews loaded, browser downloads decoded, and saved batch links reloaded. All five completed V4 thumbnails remained downloadable with bytes matching their original files. A small synthetic SHA-256 job also succeeded. The development queue ended with zero Ready and zero Unacked messages; the outbox had no unpublished rows. No development crash test was performed—the real crash and broker faults were validated only in the isolated environment above.

Do not use an automatic `down` as a data rollback. Migration 7 intentionally refuses to drop active `retry_wait` state, and the old outbox uniqueness cannot represent multiple ready generations for one job. Migration 6 down drops attempt history. If a rollback is required after V5 has written retry or attempt data, stop V5 processes, preserve a new post-upgrade backup, and restore the pre-upgrade database and matching storage copy only through a separately reviewed recovery procedure. Do not delete Docker volumes.

## Limits

The test broker shares a container with development but uses a separate vhost; live broker-stop validation is safe only when the development side is idle. PostgreSQL and local filesystem writes are separate resources, so a crash can retain an orphan attempt candidate or a missing referenced output. The download handler rejects missing or corrupt output rather than serving it. There is no automatic output reconciliation, garbage collection, or cross-machine shared storage. The V5 worker, API, and dispatcher run as local processes without an external supervisor; an exited process requires restart. A full backup restore and a production deployment were not tested here.
