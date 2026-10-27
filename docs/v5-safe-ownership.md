# V5 checkpoint 1: safe worker ownership

Checkpoint 2 now extends this foundation with [automatic recovery and bounded retries](v5-recovery.md). The details below describe the checkpoint 1 state and its observed interim limitation.

This checkpoint added ownership fencing before automatic recovery was implemented. PostgreSQL remains authoritative. During checkpoint 1, migration `00006` was applied only to the isolated `cloudforge_test` database. The development database was later upgraded to version 7 after separate approval. Older V4 workers had to be stopped before that upgrade because they do not send attempt tokens or heartbeats.

## Claim and lease

Both claim paths transition only `queued` jobs to `running` and atomically record the worker ID, a new UUID attempt ID, a lease expiration, an incremented attempt count, and a `job_attempts` row. `ClaimNext` retains `FOR UPDATE SKIP LOCKED`. PostgreSQL's `clock_timestamp()` sets and checks lease times. The default lease is 30 seconds; a worker renews every 5 seconds. `WORKER_LEASE_DURATION` may be 3 seconds to 10 minutes, and `WORKER_HEARTBEAT_INTERVAL` must be less than one third of it. Each renewal has a deadline equal to the heartbeat interval. A renewal error cancels the processor context and prevents its terminal update.

Success and failure updates require the current job ID, worker ID, attempt ID, `running` status, and an unexpired lease. They update the attempt-history row in the same transaction. An old attempt is rejected even when a new process reuses the same worker ID. A successful image result must also identify that job and its current attempt.

## Thumbnail publication

New thumbnail candidates are written to exclusive `outputs/<job_id>/<attempt_id>/thumbnail.png` paths. Different attempts cannot replace one another's files. The download handler reads only the attempt path named by the succeeded PostgreSQL result. V4 results have no `attempt_id` and continue to use `outputs/<job_id>/thumbnail.png`. Failed or uncertain candidates remain on disk for later reconciliation; this checkpoint does not delete them or add garbage collection.

## Migration and interim limit

Migration `00006` adds nullable job columns and an attempt-history table, preserving existing jobs, batches, outbox events, and results. Existing V4 `running` jobs receive an attempt ID and an immediately expired lease with a history record. They remain `running` and are **not** recovered in this checkpoint. A forced crash or a lost heartbeat can still leave a job stuck. RabbitMQ redelivery alone does not recover it; the current worker still defers `running` jobs. Existing V4 terminal results need no new fields.

Checkpoint 2 must detect expired `running` jobs, fence and classify old attempts, requeue or schedule bounded retries, create a fresh publication intent through the existing transactional outbox, and handle redispatch without relying on the original broker message. It must address output reconciliation and disk growth from retained attempt candidates. No recovery scheduler, retry state, backoff, or outbox uniqueness change is included here.

## Isolated validation

From the repository root, start the existing Compose infrastructure, run `scripts/setup-test-infrastructure.ps1` to migrate **only** `cloudforge_test`, then load `.env.test` and run targeted integration tests. The integration fixture truncates only guarded test tables, including `job_attempts`. Stop the infrastructure when validation is complete; preserve Docker volumes and development records.
