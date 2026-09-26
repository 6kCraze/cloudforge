# CloudForge V2 final architecture

## System boundary

```text
Client
  |
  v
Go API
  |
  | one PostgreSQL transaction
  v
PostgreSQL: jobs + outbox_events   <- authoritative application state
  |
  v
single outbox dispatcher
  |
  | persistent job.ready, mandatory publish, publisher confirm
  v
RabbitMQ: cloudforge.jobs -> cloudforge.jobs.v1
  |
  | manual ACK, prefetch 1
  v
single worker
  |
  +---- load / claim / finish the job in PostgreSQL
```

V1 taught the basic lifecycle by having a worker poll PostgreSQL and claim rows with `FOR UPDATE SKIP LOCKED`. That implementation remains as learning material. V2 replaces production polling with explicit work delivery: the worker waits for RabbitMQ notifications and loads the named job. This avoids repeated empty database scans and gives delivery, Unacked visibility, consumer flow control, and redelivery semantics to a broker designed for them.

RabbitMQ does not own the job lifecycle. A message says that work may be available. PostgreSQL decides whether the job is queued, running, succeeded, or failed and stores payload, result, attempt count, owner, timestamps, and application errors. This separation lets duplicate notifications be harmless and keeps API reads consistent with durable application truth.

## The database/broker dual-write problem

Creating a database row and publishing a RabbitMQ message are two independent operations. Publishing first can leave a message for a nonexistent job if the database commit fails. Committing first and then publishing directly can strand a job if the API crashes between those operations. PostgreSQL and RabbitMQ cannot share the local transaction used here.

CloudForge solves this with a transactional outbox. `POST /v1/jobs` performs one PostgreSQL transaction:

```text
BEGIN
INSERT cloudforge.jobs (... status = queued ...)
INSERT cloudforge.outbox_events (... event_type = job.ready, published_at = NULL ...)
COMMIT
```

The API returns `202` only after that commit. The job and publication intent therefore appear together or neither appears. RabbitMQ can be unavailable during this transaction without losing accepted intent.

## Normal message flow

```text
POST /v1/jobs
    -> BEGIN PostgreSQL transaction
    -> INSERT job
    -> INSERT job.ready outbox event
    -> COMMIT
    -> dispatcher selects unpublished event
    -> publish persistent job.ready with mandatory routing
    -> RabbitMQ routes to durable cloudforge.jobs.v1
    -> RabbitMQ publisher ACK confirms acceptance
    -> dispatcher records published_at
    -> worker receives with manual ACK and prefetch=1
    -> worker loads authoritative PostgreSQL job
    -> conditional queued -> running
    -> execute synthetic SHA-256 workload
    -> persist succeeded or failed
    -> ACK RabbitMQ delivery
```

The dispatcher fetches up to 32 ordered unpublished rows, closes the database query, and publishes them sequentially. It never holds a PostgreSQL transaction or row lock across broker I/O. Malformed rows are logged and left unpublished while later rows in the batch continue, so one malformed oldest row does not immediately block all following work.

## Publication guarantees

The dispatcher publishes to durable direct exchange `cloudforge.jobs` with routing key `job.ready`. Durable exchange and queue definitions survive broker restart. Persistent delivery mode asks RabbitMQ to recover Ready messages with the durable queue. These settings protected topology, binding, and a Ready message in the V2.5 restart test; they are not a substitute for production replication or backups.

Publisher confirms establish whether the broker accepted responsibility for a publication. CloudForge records `published_at` only after a positive confirmation. `mandatory=true` additionally reports a message that reached the exchange but matched no queue binding. A successful client write alone is insufficient, and neither a publisher confirm nor routing proves a consumer completed the job.

RabbitMQ confirmation and the later PostgreSQL `published_at` update are not atomic. If the dispatcher dies after confirmation but before the update, the event remains unpublished and is sent again after restart. CloudForge therefore promises **at-least-once publication**, not exactly-once delivery. Every copy carries the stable outbox event UUID as `message_id`.

## Consumption and duplicate handling

The worker declares the same topology, configures `prefetch=1`, and consumes with automatic acknowledgements disabled. One delivery remains Unacked while the worker validates it, consults PostgreSQL, claims a queued job, executes, and records a terminal result. It ACKs only after durable handling or after PostgreSQL proves execution is unnecessary.

The claim is a conditional update from `queued` to `running`. A duplicate that arrives later sees `running`, `succeeded`, or `failed` instead of authorizing another claim. Terminal duplicates are ACKed without execution, so confirmed-before-recorded duplicate publication and success-before-ACK redelivery leave `attempt_count` unchanged. PostgreSQL state, not the RabbitMQ redelivered flag or message ID alone, is the execution guard.

Malformed messages and unknown job UUIDs are logged and ACKed to prevent an unchanged poison message from creating an infinite requeue loop. V2 has no DLQ or persisted rejection record. A temporary database error is different: the worker NACKs with requeue and reconnects with capped exponential backoff because it cannot prove the delivery is complete.

## Failure windows

| Window | Result |
|---|---|
| API fails before transaction commit | Neither job nor outbox event is accepted. |
| API fails after commit/response is lost | Durable job and intent exist; the client may not know the ID. Request idempotency is not implemented. |
| Dispatcher fails before publish | `published_at` remains NULL; restart publishes it. |
| Broker fails or returns/NACKs publish | Event remains NULL; dispatcher retries after reconnect. |
| Dispatcher fails after confirm and before `published_at` | Duplicate publication can occur; worker reconciles with PostgreSQL. |
| Worker/channel fails before claim | Delivery returns to Ready; job remains queued. |
| Worker fails after terminal database write and before ACK | Delivery is redelivered; replacement sees terminal state, skips, and ACKs. |
| Worker fails after `queued -> running` but before terminal write | Delivery is redelivered; replacement refuses unsafe execution and ACKs; job remains running. |
| Database fails while handling before claim | Worker NACKs/requeues and backs off; recovery is automatic after DB returns. |
| Database fails after claim or during terminal write | Job may remain running and requires the same later recovery design. |

The full [failure matrix](v2-failure-matrix.md) records observed state, loss/duplicate behavior, and intervention for all validated scenarios.

## The intentional running-job limitation

V2 has no lease, heartbeat, or ownership expiry. After a worker claims a job, a replacement cannot distinguish an abandoned owner from a slow live owner. Re-executing could run the same side effect concurrently. V2 therefore takes the conservative choice: it ACKs a redelivery for a `running` job without executing it and leaves the PostgreSQL row visibly stuck. This is demonstrated, not hidden, and motivates a later lease and abandoned-job recovery phase.

## Precise V2 guarantees

CloudForge V2 guarantees within the tested single-dispatcher, single-worker local architecture:

- A job acknowledged with `202` and its publication intent were committed atomically in PostgreSQL.
- PostgreSQL is the authoritative source for job lifecycle and execution eligibility.
- An unpublished event remains durable across dispatcher and broker downtime.
- The dispatcher requires successful routing and a publisher confirmation before recording `published_at`.
- Publication is at least once; duplicate messages are permitted and observable through stable message IDs.
- Persistent Ready messages in the durable topology survive the tested clean RabbitMQ restart.
- Deliveries remain Unacked until durable handling or a deliberate state-based skip/drop decision.
- A completed job is not executed again when a duplicate or redelivery is handled; its attempt count remains unchanged.
- Temporary pre-claim PostgreSQL handling failures are NACKed/requeued with bounded reconnect backoff.
- Graceful worker shutdown stops new intake and allows the in-progress synthetic job to finish, persist, and ACK.

## What V2 does not guarantee

- Exactly-once message delivery.
- Exactly-once execution under every failure, especially after external side effects that are not transactionally coupled to the job row.
- Automatic recovery of abandoned `running` jobs.
- Job retries, delayed retry queues, retry scheduling, DLQ, or poison-message quarantine.
- Request idempotency if an API client loses the response after commit.
- Infinite infrastructure outage tolerance, production RabbitMQ high availability, or database disaster recovery.
- Safe multiple-dispatcher coordination.
- Multiple-worker scaling as a supported V2 operating mode, even though conditional claims defend a job row.
- Backpressure, rate limiting, cancellation, image processing, Redis, metrics, dashboards, or Kubernetes.

V2 intentionally ends at reliable delivery mechanics with known boundaries. It does not silently promote messaging infrastructure into canonical application state.
