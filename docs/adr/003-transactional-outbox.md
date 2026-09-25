# ADR 003: Record RabbitMQ publication intent with a transactional outbox

## Context

CloudForge must eventually publish a notification after accepting a job. Committing PostgreSQL and publishing to RabbitMQ are two independent writes. A crash or unavailable broker between them can produce a committed job with no notification. Publishing first can produce a notification for a database transaction that later rolls back.

PostgreSQL and RabbitMQ do not share the API's local ACID transaction.

## Decision

Create the job and one `job.ready` outbox event in the same PostgreSQL transaction. Return `202 Accepted` only after both inserts commit. Store only the job ID in the future message body; keep the event ID and event type as outbox columns.

PostgreSQL remains authoritative for lifecycle state. The outbox is durable publication intent. RabbitMQ will later transport notifications.

## Alternatives considered

- **Commit then publish directly:** can strand a committed job when publication fails.
- **Publish then commit:** can expose a message for missing or rolled-back state.
- **Use RabbitMQ as the job state store:** loses the canonical relational lifecycle and result history.
- **Distributed transaction protocol:** adds operational complexity and is not a practical fit for this project.

## Consequences

Job creation gains one database insert but no broker dependency. A later dispatcher must publish unpublished rows and mark them only after broker confirmation. A dispatcher crash between broker confirmation and the PostgreSQL update can cause duplicate delivery, so workers must always consult authoritative job state. V2.2 intentionally stops before implementing that dispatcher.
