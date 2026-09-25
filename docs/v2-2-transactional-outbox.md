# V2.2 transactional outbox

## The dual-write problem

PostgreSQL and RabbitMQ are separate systems and cannot participate in one ordinary local ACID transaction. If the API commits a job and then fails to publish, the durable job exists without a work notification. If it publishes first and the database transaction later fails, a worker can receive a notification for a job that does not durably exist.

V2.2 records publication intent in PostgreSQL alongside the job:

```text
BEGIN
  INSERT jobs
  INSERT outbox_events
COMMIT
```

The API returns `202 Accepted` only after that commit succeeds. A failure in either insert rolls the entire transaction back. RabbitMQ availability has no effect on this transaction.

## Temporary checkpoint architecture

```text
Client
  |
  v
API
  |
  | one PostgreSQL transaction
  v
+--------------------------------+
| PostgreSQL                     |
|  jobs                          |
|  outbox_events                 |
+--------------------------------+
             ^
             |
       polling worker

RabbitMQ runs locally but has no CloudForge application connection.
```

PostgreSQL remains authoritative for job lifecycle. `jobs` contains the state, payload, result, and application failure. `outbox_events` contains a durable intent to publish a notification later. RabbitMQ will transport that notification in later checkpoints.

## Event shape

Each job creates exactly one `job.ready` event. The payload is deliberately small:

```json
{"job_id":"<job UUID>"}
```

The event ID remains the outbox primary key and can become RabbitMQ `message_id` metadata in V2.3. The event type remains a column and can drive the routing key. The complete job input is not copied into the event because workers must load authoritative job data from PostgreSQL.

## Unpublished index

V2.3 will search for unpublished events in creation order:

```sql
WHERE published_at IS NULL
ORDER BY created_at, id
```

The partial `(created_at, id)` index contains only unpublished rows. Published history does not enlarge the dispatcher lookup index, and no additional indexes are needed for the upcoming query.

## Expected V2.2 state

The V1 polling worker can complete a new job without touching RabbitMQ. Until V2.3 exists, the corresponding event remains unpublished:

```text
jobs.status                  succeeded
outbox_events.event_type     job.ready
outbox_events.published_at   NULL
```

This is the correct temporary state, not a failed publication.
