# ADR 001: Go for the backend

## Context

CloudForge is a learning project about background jobs, concurrency, worker failure, and service behavior under load. The implementation should expose those concepts directly and remain easy to run locally.

## Decision

Use Go for the API, migration command, and eventual workers. Begin with the standard library where practical and add focused packages for PostgreSQL and migrations.

## Alternatives considered

- **Node.js/TypeScript:** familiar and productive, but CPU-intensive image work would require separate execution strategies earlier.
- **Python:** strong image tooling, but interpreter and process-model details would complicate the first concurrency lessons.

## Consequences

Go gives us explicit contexts, goroutines, a small deployment artifact, and good performance. We must still design concurrency, cancellation, and delivery semantics carefully; the language does not make distributed work reliable by itself.

