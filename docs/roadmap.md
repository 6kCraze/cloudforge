# CloudForge roadmap

CloudForge is built in small versions. Each phase introduces a system concern only after the previous architecture makes its need visible. PostgreSQL remains the authoritative source of job lifecycle state throughout; RabbitMQ later handles work delivery.

| Version | Goal | Proof |
| --- | --- | --- |
| V0 | Go module, local PostgreSQL via Compose, configuration, connectivity, migrations, and ADRs | Fresh local setup; migration up/down/up; normal and integration tests |
| V1 | One API and one PostgreSQL-polling worker with a synthetic checksum job; `FOR UPDATE SKIP LOCKED` | Submit, run, and inspect a job; demonstrate a worker crash leaving `running` stuck |
| V2 | RabbitMQ, transactional outbox, dispatcher, and manual acknowledgements | Jobs committed while RabbitMQ is unavailable publish later |
| V3 | Multiple workers on the synthetic workload; containerize API, dispatcher, and workers | Scale workers with Compose and observe concurrent processing |
| V4 | Image batches and thumbnail generation first; add other image operations afterward | Real uploaded batch produces verified thumbnails |
| V5 | Attempt history, retry classification, backoff, delayed routing, and messaging DLQ | Transient failures retry; terminal application failures are recorded in PostgreSQL |
| V6 | Worker heartbeats, leases, recovery, and idempotent processing | Kill a worker and recover unfinished work without duplicate visible output |
| V7 | Graceful cancellation and later `cancelled` state | Queued or waiting jobs cancel; running work stops and records cancellation |
| V8 | Priorities and backpressure | Bounded queues and fair progress under overload |
| V9 | Structured logs, correlation IDs, Prometheus, and Grafana | Trace a request through job, batch, worker, attempt, and metrics |
| V10 | Next.js operational dashboard | Real batches, jobs, workers, failures, and metrics displayed |
| V11 | k6 benchmarks and scripted failure experiments | Measured worker-scale results and recovery report |
| V12 | Complete reproducible Compose stack, CI, documentation, and demo artifacts | Clean checkout runs the stack, tests, and demos |
| V13 | Optional MinIO, deployment, tracing, or Kubernetes when justified | Each addition addresses a documented problem |

The durable job states begin in V1 as `queued`, `running`, `succeeded`, and `failed`. V5 introduces `retry_wait`; V7 adds `cancelled`. A PostgreSQL `failed` job is an application outcome. A RabbitMQ dead-letter message is a delivery-system artifact, so it must be reconciled against PostgreSQL rather than treated as canonical job history. Redis remains deferred until a measured use case exists.

