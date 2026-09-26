# ADR 005: Consume job notifications with manual acknowledgement

## Status

Accepted for V2.4.

## Context

V2.3 provides at-least-once RabbitMQ publication, including an intentional duplicate window. Auto-acknowledgement could remove a delivery before its job result is durable. RabbitMQ delivery state also cannot replace PostgreSQL lifecycle state.

## Decision

The worker consumes `cloudforge.jobs.v1` with manual ACK and prefetch one. It validates the notification, loads the job from PostgreSQL, and atomically claims only `queued` jobs by ID. It ACKs after a durable terminal result, after recognizing a terminal duplicate, or after deliberately deferring a `running` job. Invalid and unknown messages are logged and ACKed to prevent infinite requeue loops. Temporary database handling errors are NACKed with requeue and cause consumer reconnection.

PostgreSQL remains authoritative. Duplicate safety comes from the conditional `queued` to `running` database update, not from RabbitMQ uniqueness.

## Consequences

Worker or connection failure before ACK causes redelivery. A succeeded or failed job safely absorbs that duplicate. A job left `running` cannot be recovered safely in V2.4 and remains stuck, because leases and ownership expiry are intentionally deferred. Prefetch one limits each worker to one unacknowledged delivery and makes this behavior easy to observe at the cost of throughput.
