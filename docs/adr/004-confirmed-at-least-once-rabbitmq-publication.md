# ADR 004: Publish outbox events to RabbitMQ at least once

## Context

V2.2 records job creation and publication intent atomically in PostgreSQL. V2.3 must move committed intent to RabbitMQ without holding a database transaction open across network I/O. A successful client write is insufficient evidence that the broker accepted a message, and broker acceptance alone does not prove a binding routed it to the intended queue.

## Decision

Run one outbox dispatcher. Select a small ordered batch of unpublished rows without a long-lived lock and process it sequentially. Log and retain malformed rows while continuing to valid later rows in the batch. Publish each valid event as a persistent message with `mandatory=true`, wait for a publisher confirmation, reject returned/unroutable publications, and set `published_at` only after a routed ACK. Declare the durable exchange, queue, and binding on every connection. Reconnect with capped exponential backoff.

Use the outbox UUID as AMQP `message_id`, `job.ready` as both event type and routing key, and a body containing only `job_id`.

## Alternatives considered

- **Hold a PostgreSQL transaction open through publication:** couples row locks and connection usage to broker latency and outages.
- **Mark before publishing:** can permanently lose a notification after a crash.
- **Mark after the client publish call without a confirm:** can record publication the broker never accepted.
- **Treat an ACK as proof of routing:** ignores mandatory returns when no queue binding matches.
- **Distributed transaction or exactly-once claim:** adds substantial coordination and still does not provide exactly-once job execution.

## Consequences

A crash after the broker ACK and before `published_at` creates a duplicate publication on restart. This is intentional at-least-once behavior. The future consumer must tolerate the same `message_id` more than once and use PostgreSQL job state to decide whether work remains. V2.3 keeps a single dispatcher; multiple dispatchers require later coordination.
