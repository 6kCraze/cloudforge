# V1 architecture: PostgreSQL polling

## Scope

V1 adds the smallest complete background-job path: an HTTP API persists a validated job, one worker claims it from PostgreSQL, deterministic work runs outside the claim transaction, and the result or application failure is persisted. There is no RabbitMQ, Redis, retry scheduler, lease, cancellation, image processing, or worker container yet.

PostgreSQL is the authoritative source of truth for the job lifecycle. Every externally visible state comes from the `cloudforge.jobs` row.

## Lifecycle

```text
POST /v1/jobs
      |
      v
   queued -- atomic claim --> running -- work succeeds --> succeeded
                                  |
                                  +---- work fails -----> failed
                                  |
                                  +---- worker crash ---> running forever (intentional V1 limitation)
```

V1 permits only `queued`, `running`, `succeeded`, and `failed`. `retry_wait` arrives with retry reliability. `cancelled` arrives later, after reliability is working.

## API

`POST /v1/jobs` accepts:

```json
{
  "type": "synthetic.sha256",
  "payload": {
    "text": "hello CloudForge",
    "iterations": 100000
  }
}
```

It validates the exact job type, nonempty UTF-8 text up to 64 KiB, and an iteration count from 1 through 50,000,000. It persists the row before returning `202 Accepted`:

```json
{"id":"28a3e40d-d025-4c22-b142-e359982e3f39","status":"queued"}
```

`GET /v1/jobs/{id}` reads the canonical PostgreSQL row. Unknown valid IDs return `404`; malformed IDs and invalid requests return structured `400` errors.

## Atomic claim transaction

The worker claims work using one short database transaction:

1. Begin a transaction.
2. Select the oldest `queued` row with `FOR UPDATE SKIP LOCKED`.
3. Update that locked row to `running`, increment `attempt_count`, set `claimed_by`, and record `started_at`.
4. Commit.
5. Run the synthetic workload outside the transaction.
6. Update the same owned `running` row to `succeeded` with a result or `failed` with an application error.

A naive `SELECT` followed later by `UPDATE` allows two workers to read the same queued job. `FOR UPDATE` gives the selected row an exclusive lock. `SKIP LOCKED` makes another concurrent worker immediately skip a row already being claimed instead of waiting for it and potentially processing it after the first transaction completes. The claim transaction stays short because computation occurs after commit.

V3 can run multiple workers against this same claim primitive. The integration suite already races two claimers and verifies that exactly one receives the single queued row.

## Synthetic workload

`synthetic.sha256` hashes the input text once, then repeatedly hashes the preceding 32-byte digest until the requested iteration count is reached. The result records the lowercase hexadecimal digest, the iteration count, and elapsed milliseconds. The digest is deterministic; elapsed time is observational.

Bounds keep a request from consuming unlimited memory or CPU. They are application validation and are repeated before execution so malformed database data fails safely.

## Shutdown and crash behavior

Ctrl+C cancels the polling loop. Once a job is claimed, the worker deliberately finishes it and persists its terminal state before stopping. It does not claim another job after shutdown begins.

A forced crash cannot execute that cleanup. Because V1 has no lease expiry or recovery process, the committed `running` row stays `running` and replacement workers skip it. This is covered by `TestClaimWithoutCompletionRemainsRunning` and can be demonstrated manually using the README instructions. V2 will add the reliability mechanism only after this failure has been observed.

## Failure ownership

Application execution failures are written to PostgreSQL as `failed`, with `error_code`, `error_message`, and `completed_at`. A future RabbitMQ dead-letter queue is separate messaging infrastructure: a dead-letter message records a delivery problem and is never the canonical application job state.

Future observability will add structured correlation fields including `job_id`, `batch_id`, `worker_id`, `attempt_id`, and `request_id`. V1 logs the available job and worker identity but does not prematurely add the later schema.
