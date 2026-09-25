# V2.3 outbox dispatcher

## Architecture

```text
API -- one PostgreSQL transaction --> jobs + outbox_events
                                             |
                                      cmd/dispatcher
                                             |
                              mandatory persistent publish
                                             |
                              cloudforge.jobs exchange
                                  job.ready binding
                                             |
                              cloudforge.jobs.v1 queue

V1 polling worker <---------------------- PostgreSQL
```

PostgreSQL remains authoritative for job lifecycle, payload, results, and application failures. RabbitMQ transports a small notification that identifies a job. The V1 worker still polls PostgreSQL in this checkpoint, so it may finish a job while that job's RabbitMQ message remains ready with no consumer.

## Topology and durability

The dispatcher declares a durable direct exchange named `cloudforge.jobs`, a durable queue named `cloudforge.jobs.v1`, and a `job.ready` binding. Matching redeclarations are idempotent, so reconnecting processes can safely verify the topology.

A durable exchange retains its definition after restart. A durable queue retains its definition. A persistent message requests disk-backed recovery for the message. These settings improve broker-restart survival, but they do not create exactly-once delivery, make PostgreSQL and RabbitMQ atomic, or prove that a consumer completed work.

There are intentionally no retry queues, dead-letter queues, priorities, or workload-specific queues.

## Message contract

The routing key and AMQP type are `job.ready`. The body is:

```json
{"job_id":"<job UUID>"}
```

AMQP metadata:

| Property | Value |
| --- | --- |
| `message_id` | outbox event UUID |
| `type` | `job.ready` |
| `content_type` | `application/json` |
| `delivery_mode` | persistent (`2`) |
| `timestamp` | outbox `created_at` |
| `app_id` | `cloudforge-dispatcher` |
| `schema_version` header | integer `1` |

The full job payload is deliberately absent. A future worker uses `job_id` to load authoritative data from PostgreSQL.

## Confirmation lifecycle

The dispatcher performs a short `SELECT` using the existing partial index. It fetches at most 32 rows, then closes the query before any RabbitMQ I/O:

```sql
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT 32
```

It does not open an explicit transaction or lock rows while publishing. Events remain serial: each valid event publishes with `mandatory=true`, then waits up to `DISPATCHER_CONFIRM_TIMEOUT` for a publisher confirmation before the next event begins.

If validation fails, the dispatcher logs the event ID and validation error, leaves `published_at` NULL, and continues through the rest of the fetched batch. This prevents one malformed oldest row from being selected alone forever and blocking every later event. No malformed event is deleted or silently treated as published.

- A publisher ACK means RabbitMQ accepted responsibility for the publication under its configured durability rules.
- A publisher NACK means RabbitMQ did not accept responsibility, so `published_at` stays NULL.
- A successful Go `Publish` call only means the client handed the frame to its connection; it is not a broker confirmation.
- A mandatory `basic.return` means the exchange accepted the message but no binding routed it to a queue. That also leaves `published_at` NULL.
- Publisher confirms and returns do not mean a consumer received or processed the message.

After a routed ACK, the dispatcher runs:

```sql
UPDATE cloudforge.outbox_events
SET published_at = now()
WHERE id = $1
  AND published_at IS NULL;
```

The NULL condition prevents overwriting the original publication timestamp and exposes unexpected competing updates. Rows are retained as publication history.

## At-least-once duplicate window

RabbitMQ and PostgreSQL cannot share this update atomically:

```text
RabbitMQ accepts and confirms event
              |
        dispatcher crashes
              |
published_at remains NULL
              |
dispatcher restarts and republishes event
```

Both messages have the same outbox UUID in `message_id`. This is expected at-least-once publication. V2.4 must treat notifications as hints, consult PostgreSQL, and safely handle duplicate deliveries. No distributed transaction is attempted.

## Reconnection and shutdown

If connection, channel, publication, return, or confirmation handling fails, the dispatcher leaves the event unpublished, closes the old publisher, and reconnects with exponential backoff from `DISPATCHER_BACKOFF_MIN` up to `DISPATCHER_BACKOFF_MAX`. Each successful connection redeclares the topology and enables confirm mode. An empty outbox sleeps for `DISPATCHER_POLL_INTERVAL` instead of hammering PostgreSQL.

Ctrl+C cancels connection attempts, polling, confirmation waits, and backoff. If confirmation has already arrived, the dispatcher makes one short uncancelled attempt to record `published_at` before stopping.

There is exactly one dispatcher in V2.3. Multiple dispatchers would need database coordination such as claims or leases; they are out of scope.

## Broker-outage demonstration

Run the API in one PowerShell terminal:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/api
```

Stop RabbitMQ, then submit a job from another terminal:

```powershell
docker compose stop rabbitmq

$body = @{
  type = 'synthetic.sha256'
  payload = @{ text = 'broker outage'; iterations = 100000 }
} | ConvertTo-Json

$job = Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/jobs `
  -ContentType 'application/json' -Body $body
$job
```

Start the dispatcher while RabbitMQ is still stopped:

```powershell
. .\scripts\load-env.ps1
go run ./cmd/dispatcher
```

Its structured logs show failed connection attempts and increasing retry delays. The job and outbox row exist, but `published_at` remains NULL. Restore the broker:

```powershell
docker compose up -d --wait rabbitmq
```

The dispatcher reconnects, redeclares topology, publishes, receives an ACK, and records `published_at`. No intent was lost.

## Duplicate-window demonstration

The integration test `TestConfirmedButUnrecordedEventCanBePublishedTwice` safely demonstrates the crash window by publishing one still-unpublished event twice and asserting that both messages have the same `message_id`.

For a manual local demonstration, stop the dispatcher after it publishes an event, set only that test event's `published_at` back to NULL, then restart the dispatcher. This simulates the database state left by a crash after broker confirmation:

```sql
UPDATE cloudforge.outbox_events
SET published_at = NULL
WHERE id = '<test event UUID>';
```

The ready count increases again. Use this only with an event created for the demonstration.

## RabbitMQ Management UI

Open [http://localhost:15672](http://localhost:15672) and sign in with the local credentials from `.env`.

1. Open **Exchanges**, then `cloudforge.jobs`, to inspect the durable direct exchange and its `job.ready` binding.
2. Open **Queues and Streams**, then `cloudforge.jobs.v1`, to inspect the durable queue.
3. The queue overview shows **Ready**, **Unacked**, **Total**, **Consumers**, and message rates.
4. In V2.3, **Consumers** and **Unacked** remain zero. **Ready** increases as the dispatcher publishes because V2.4 consumption does not exist yet.

Avoid using **Get messages** during the observation unless you intend to remove or requeue a message.

## Inspection queries

Unpublished events:

```sql
SELECT id, event_type, aggregate_id, payload, created_at, published_at
FROM cloudforge.outbox_events
WHERE published_at IS NULL
ORDER BY created_at, id;
```

Published events:

```sql
SELECT id, event_type, aggregate_id, payload, created_at, published_at
FROM cloudforge.outbox_events
WHERE published_at IS NOT NULL
ORDER BY created_at, id;
```

Matching job and event state:

```sql
SELECT j.id, j.status, j.attempt_count, j.claimed_by,
       e.id AS event_id, e.event_type, e.published_at
FROM cloudforge.jobs AS j
JOIN cloudforge.outbox_events AS e ON e.aggregate_id = j.id
ORDER BY j.created_at, j.id;
```

## Known V2.3 limitations

- Only one dispatcher instance may run. There are no claims or leases for coordinating a dispatcher pool.
- Up to 32 events are fetched per database query, but publication remains sequential. There is no concurrent publication or `LISTEN/NOTIFY` wakeup.
- Malformed events remain unpublished and are reconsidered in later polls. If 32 or more malformed events occupy an entire oldest batch, newer valid events can still be blocked until an operator corrects at least one row. A later version can add explicit outbox failure handling without hiding the evidence now.
- Published rows are retained without an archival policy.
- No CloudForge RabbitMQ consumer exists. Manual ACK/NACK, prefetch, and duplicate-safe job claiming begin in V2.4.
