# V3 controlled multiple-worker scaling

## Architecture

```text
                              RabbitMQ
                                 |
                      cloudforge.jobs.v1
                                 |
                +----------------+----------------+
                |                                 |
                v                                 v
            worker-01                         worker-02
                |                                 |
                +----------------+----------------+
                                 |
                                 v
                    PostgreSQL authoritative state
```

V3 runs PostgreSQL and RabbitMQ in Docker while the API, dispatcher, and workers run as separate local Go processes. Application containerization is deferred until it materially improves reproducibility, deployment, or scaling. V3 validates concurrency correctness with the existing production architecture.

Both workers consume from the same durable queue. RabbitMQ uses the competing-consumer model: it assigns a delivery to one available consumer instead of broadcasting every message to every worker. Distribution may vary with consumer readiness, acknowledgements, connection timing, and workload duration, so CloudForge does not promise strict round-robin fairness.

One queue is the correct design because each job should be handled by one worker and all workers offer the same synthetic capability. A queue per worker would require the producer to choose a worker, fragment pending work, and make capacity changes harder.

## Identity, prefetch, and execution authority

Each process has a distinct `WORKER_ID`, such as `worker-01` or `worker-02`. The identity appears in structured logs, RabbitMQ consumer tags, and PostgreSQL `claimed_by`. When `WORKER_ID` is omitted, the worker falls back to hostname plus process ID, but explicit names make local demonstrations easier to understand.

Every worker keeps `prefetch=1`, so it owns at most one Unacked delivery. A busy worker cannot reserve a large group of messages while another worker is idle. This is useful for visible distribution of longer jobs; V3 does not tune or benchmark other prefetch values.

RabbitMQ distribution is not the correctness guard. Every delivery names a PostgreSQL job, and the worker claims it with a conditional transition:

```sql
UPDATE cloudforge.jobs
SET status = 'running',
    claimed_by = $2,
    attempt_count = attempt_count + 1
WHERE id = $1
  AND status = 'queued';
```

If two workers receive duplicate notifications, only one update can change `queued` to `running`. The loser reloads authoritative state and skips execution. A completed duplicate is also ACKed without incrementing `attempt_count`.

## Deterministic automated validation

`TestV3TwoWorkersShareQueueSafely` uses two real RabbitMQ consumers and three small subtests:

1. Two jobs reach two worker-specific blocking processors. Both remain `running` until one release signal, proving overlap without CPU-heavy SHA work. PostgreSQL records different `claimed_by` values and attempt 1.
2. Two copies of one event reach two workers. A test gate pauses each worker after its existing production queued read. Releasing both gates exercises the real conditional `ClaimByID`: exactly one succeeds and one returns `ErrNotClaimable`. The winning processor blocks while the test checks one execution and attempt 1. No artificial preliminary read or production change is added.
3. Two consumers connect, `worker-02` shuts down gracefully, Consumers falls to one, and a later job completes through `worker-01`.

Each worker registers cleanup before execution. Cleanup releases the idempotent test gates before cancellation and a bounded shutdown wait. Gates and observation sends have time limits, including on failed assertions.

Each subtest observes successful calls to the real AMQP acknowledger (two for the concurrent and duplicate cases, one for the remaining worker). ACKs do not themselves receive publisher confirms. The test then closes both actual consumer channels and checks Consumers=0 and Ready=0: any outstanding Unacked delivery would be requeued at channel close. Durable job results and attempt counts are checked separately. Ready=0 alone is never treated as proof.

All fixtures use the isolated [integration-test setup](integration-testing.md), including older V2 fixtures.

Existing V2 tests continue to cover prefetch one, success-before-ACK redelivery, Unacked reassignment, completed duplicates, graceful shutdown, and the intentional abandoned-running limitation.

## Broker ownership and application ownership

RabbitMQ owns delivery state. A delivery is Ready, Unacked, requeued, redelivered, or ACKed. If a worker disappears before ACK, RabbitMQ can assign that delivery again.

PostgreSQL owns application state. A job is queued, running, succeeded, or failed. Broker redelivery cannot prove that a `running` job is abandoned. If a worker dies after claiming a job, the replacement sees `running`, refuses unsafe re-execution, and ACKs the delivery under the existing policy. The job remains visibly stuck until a later lease, heartbeat, and abandoned-job recovery phase.

Stopping one worker therefore has two outcomes:

- Available or pre-claim deliveries can be handled by another worker.
- Work already transitioned to `running` can remain stuck if its owner crashes before terminal persistence.

## Lightweight manual demonstration

Start existing infrastructure from the repository root:

```powershell
docker compose up -d --wait db rabbitmq
```

Open four PowerShell terminals, change to the repository root, and load `.env` in each.

API:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/api
```

Dispatcher:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/dispatcher
```

Worker 1:

```powershell
. .\scripts\load-env.ps1
$env:WORKER_ID = "worker-01"
go run ./cmd/worker
```

Worker 2:

```powershell
. .\scripts\load-env.ps1
$env:WORKER_ID = "worker-02"
go run ./cmd/worker
```

Submit four moderate jobs from a fifth terminal:

```powershell
$jobs = 1..4 | ForEach-Object {
    $body = @{
        type = "synthetic.sha256"
        payload = @{
            text = "V3 multi-worker demo job $_"
            iterations = 5000000
        }
    } | ConvertTo-Json

    Invoke-RestMethod -Method Post `
        -Uri "http://localhost:8080/v1/jobs" `
        -ContentType "application/json" `
        -Body $body
}
$jobs
```

This is a correctness demonstration, not a benchmark. Exact distribution is intentionally unspecified; both workers receiving at least one job is sufficient.

## Observed V3 validation

The resource-limited manual validation on 2026-09-26 used exactly two workers and four jobs with 5,000,000 SHA-256 iterations each. RabbitMQ reported two consumers before submission. All jobs succeeded with `attempt_count = 1`:

| Worker | Jobs | Observed job durations |
|---|---:|---|
| `worker-01` | 2 | 840 ms and 872 ms |
| `worker-02` | 2 | 838 ms and 870 ms |

PostgreSQL showed two separate overlapping pairs. The first pair overlapped from `05:04:56.073014+00` through `05:04:56.911252+00`; the second overlapped from `05:04:56.915561+00` through `05:04:57.783805+00`. These are correctness observations from one local run, not benchmark results or throughput claims.

After `worker-02` stopped gracefully, RabbitMQ reported Consumers=1, Ready=0, and Unacked=0. The deterministic automated worker-stop subtest then provides the stronger behavioral proof: with only `worker-01` connected, a newly published job completed through that remaining worker.

The September validation passed normal tests, vet, integration tests, and an untagged race run. That race run did **not** include the integration-tagged V3 test. The October correction report below records current validation separately.

## RabbitMQ Management UI

Open [http://localhost:15672](http://localhost:15672), sign in with the local credentials, and select **Queues and Streams** → `cloudforge.jobs.v1`.

- **Consumers = 2** means both worker processes are connected.
- **Ready** is work waiting for assignment.
- **Unacked** is work currently owned by consumers but not yet durably handled and ACKed.
- During two long-enough jobs, Unacked may briefly reach two because prefetch is one per consumer.
- After stopping `worker-02`, Consumers should fall to one while `worker-01` remains operational.

The UI samples periodically, so a short job can pass through Ready or Unacked between graph updates. PostgreSQL timestamps provide the deterministic concurrency evidence.

## PostgreSQL inspection

Recent jobs:

```sql
SELECT id, status, claimed_by, attempt_count,
       started_at, completed_at,
       completed_at - started_at AS execution_time
FROM cloudforge.jobs
ORDER BY created_at DESC
LIMIT 20;
```

Worker distribution for completed work:

```sql
SELECT claimed_by, COUNT(*) AS jobs,
       MIN(started_at) AS first_started,
       MAX(completed_at) AS last_completed
FROM cloudforge.jobs
WHERE claimed_by IS NOT NULL
GROUP BY claimed_by
ORDER BY claimed_by;
```

Running jobs:

```sql
SELECT id, claimed_by, attempt_count, started_at,
       now() - started_at AS running_for
FROM cloudforge.jobs
WHERE status = 'running'
ORDER BY started_at;
```

Overlap between recent jobs:

```sql
SELECT a.id AS job_a, a.claimed_by AS worker_a,
       b.id AS job_b, b.claimed_by AS worker_b,
       GREATEST(a.started_at, b.started_at) AS overlap_started,
       LEAST(a.completed_at, b.completed_at) AS overlap_ended
FROM cloudforge.jobs AS a
JOIN cloudforge.jobs AS b ON a.id < b.id
WHERE a.started_at < b.completed_at
  AND b.started_at < a.completed_at
ORDER BY overlap_started DESC;
```

## V3 boundaries

V3 adds no worker registry, leases, heartbeats, retries, DLQ, priorities, backpressure, autoscaling, image processing, or formal performance claims. It supports controlled local scaling through independent worker processes and keeps the known running-job crash limitation visible.

## V3 correction review: 2026-10-12

V3 is ready for commit review. No production API, dispatcher, worker, claiming logic, migration, or Compose definition changed. Nothing has been committed or pushed, and V4 has not been implemented.

### Exact files in the V3 change set

- `.gitignore`: keep test credentials ignored; track the example.
- `.env.test.example`: separate explicit local test URLs.
- `scripts/load-test-env.ps1`: load only test URLs, clear stale inherited values, fail on missing configuration.
- `scripts/setup-test-infrastructure.ps1`: idempotent setup/migration of test namespaces in existing containers.
- `internal/testenv/config.go`: reject absent/unsafe targets before connecting; no development fallback.
- `internal/testenv/config_test.go`: positive configuration, missing configuration, and unsafe-target checks.
- `internal/database/postgres_integration_test.go`: isolated fixture configuration.
- `internal/jobs/store_integration_test.go`: isolated configuration; missing configuration fails instead of skipping.
- `internal/dispatcher/dispatcher_integration_test.go`: isolated fixture configuration.
- `internal/worker/worker_integration_test.go`: isolated fixture configuration.
- `internal/system/v2_integration_test.go`: isolated fixture configuration.
- `internal/system/v3_multi_worker_integration_test.go`: deterministic two-worker proofs, real duplicate claim competition, failure-safe cleanup, ACK observations and settled queue checks.
- `README.md`: V3 description, consolidated roadmap, and safe integration instructions.
- `docs/integration-testing.md`: reproducible setup and resource-limited validation commands.
- `docs/v2-5-validation-report.md`: current test configuration instructions beside historical results.
- `docs/roadmap.md`: consolidated mandatory V0–V6 plan and brief V4 sequence; further infrastructure optional.
- `docs/v3-multiple-workers.md`: V3 behavior, preserved demonstration evidence, and this correction report.

The local ignored `.env.test` was also created from the example. It is not part of the commit.

### Isolation and preservation evidence

Before any destructive fixture ran, PostgreSQL confirmed `current_database=cloudforge_test` and `current_user=cloudforge_test` for the test target. The test role has no superuser, create-database, or create-role privileges. In the development database, its schema USAGE and TRUNCATE privileges on both jobs and outbox are false. RabbitMQ lists permissions for the test account only in `cloudforge_test`, with no management tags.

Development jobs and outbox rows were snapshotted in deterministic ID order before and after validation. Both SHA-256 hashes were:

```text
15D0B030483F6A0A7C03B777ADDCCDA27A718C4326B42F1E7FDA8E17D38F18DD
```

The four preserved September demo jobs still exist, all succeeded at attempt 1, with unchanged owners and timing:

| Job ID | Worker |
|---|---|
| `4511b5e8-b03a-44fe-89de-86e6959d348c` | `worker-01` |
| `3423f941-16e5-4fb3-a69c-057bc71bedaa` | `worker-02` |
| `4ebb61f4-8424-459c-846b-0af44e04f05d` | `worker-02` |
| `c07c66a0-977f-4abc-a43e-1af5086b81b8` | `worker-01` |

The development queue stayed Ready=0, Unacked=0, Consumers=0. The test queue also ended 0/0/0. Existing volumes remain `cloudforge_postgres_data` (created September 23) and `cloudforge_rabbitmq_data` (created September 25). Compose restarted/recreated its two existing service containers using those volumes; no additional database/broker service or image pull was used.

### Commands actually executed and outcomes

Commands ran sequentially. Full reproduction commands, including the bounded Docker race invocation, are in [integration-testing.md](integration-testing.md).

| Command/check | Outcome |
|---|---|
| `go test -p 1 ./internal/testenv` | Pass: valid explicit targets accepted; missing/development/remote/override targets rejected |
| V3 targeted command with both TEST URLs absent | Expected immediate failure before connecting; no fallback |
| `go test -p 1 -tags=integration ./internal/system -run '^TestV3TwoWorkersShareQueueSafely$' -count=1 -v -timeout=60s` | Pass: all three subtests; corrected test body about 0.53 s |
| `go test -p 1 -tags=integration ./internal/database ./internal/jobs ./internal/dispatcher ./internal/worker ./internal/system -count=1 -timeout=120s` | All five affected packages pass; validates every migrated fixture |
| Cached `golang:1.27` with `--cpus=1 --memory=2g`, `GOPROXY=off`; `go test -p 1 -race -tags=integration ./internal/system -run '^TestV3TwoWorkersShareQueueSafely$' -count=1 -v -timeout=60s` | Pass, no race report; all three integration subtests included |
| Temporary assertion failure after both processors block, targeted concurrent subtest | Expected failure exits in under one second; cleanup releases processors and stops workers |
| Temporary assertion failure after both queued reads block, targeted duplicate subtest | Expected failure exits in under one second; cleanup releases claim/processor gates and stops workers |
| `go vet -p 1 -tags=integration ./internal/testenv ./internal/database ./internal/jobs ./internal/dispatcher ./internal/worker ./internal/system` | Pass |
| Setup script rerun | Pass; existing namespaces reused, no migrations pending |
| Development snapshot comparison and broker queue inspection | Identical job/outbox data; both namespaces settled at 0/0/0 |
| `git diff --check` | Pass |

The temporary assertion injections were restored in `finally` blocks; no intentionally failing test remains. The final source is the same source that passed the normal and race runs. Unaffected broad suites and the CPU-heavy manual demo were not repeated.

The concurrent subtest proved two distinct owners and overlapping persisted intervals. The duplicate subtest synchronized both actual queued reads, observed exactly one successful conditional claim and one `ErrNotClaimable`, and recorded one execution and attempt 1. Successful real ACK calls were observed in counts 2, 2, and 1 respectively; closing actual consumer channels afterward left no Ready work to be requeued. Durable results were read back from PostgreSQL in every subtest.

Temporary workers and the race container exited. PostgreSQL and RabbitMQ were returned to their original stopped state after validation; volumes and demonstration rows remain available for later inspection.

### Remaining limits

- V3 proves the tested two-worker cases, not strict distribution fairness, a throughput benchmark, or exactly-once execution across crashes.
- Forced crashes after claiming can still leave jobs running. Lease/fencing/recovery and reliable redispatch belong to V5.
- One dispatcher remains supported. Published-outbox retention and a full batch of malformed oldest events remain existing limitations.
- Test fixtures share one isolated namespace and must serialize. Customized installations must keep test-account grants restricted to that namespace.
- V4 remains a proposal: one bounded JPEG/PNG upload through durable intent, broker, thumbnail worker, persisted output, and download; then up to four-image batches and a minimal browser UI. Preserve SHA-256 jobs and use same-machine bounded filesystem storage.
