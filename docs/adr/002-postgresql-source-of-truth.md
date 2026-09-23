# ADR 002: PostgreSQL as the durable source of truth

## Context

Jobs must survive process restarts and have an inspectable lifecycle. Later, RabbitMQ will deliver work, but a queue message alone cannot represent all application state, attempts, results, and audit history.

## Decision

Store canonical job state in PostgreSQL. V0 establishes connectivity and migrations. V1 will introduce the first jobs table and database polling. Later broker messages will carry job IDs and trigger work, while workers confirm and update state in PostgreSQL.

## Alternatives considered

- **RabbitMQ or Redis as the only state store:** simpler-looking initial setup, but not a durable, queryable application history.
- **SQLite:** useful for a single process, but a weaker fit for concurrent claims and later distributed workers.

## Consequences

PostgreSQL transactions can enforce state transitions and support `FOR UPDATE SKIP LOCKED`. We must plan indexes, connection limits, migrations, and recovery when database access is interrupted.

