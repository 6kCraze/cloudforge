# CloudForge roadmap

CloudForge is built in small versions. Each phase introduces a system concern only after the previous architecture makes its need visible. PostgreSQL remains the authoritative source of job lifecycle state throughout; RabbitMQ later handles work delivery.

| Version | Goal | Proof |
| --- | --- | --- |
| V0 | Go module, local PostgreSQL via Compose, configuration, connectivity, migrations, and ADRs | Fresh local setup; migration up/down/up; normal and integration tests |
| V1 | One API and one PostgreSQL-polling worker with a synthetic checksum job; `FOR UPDATE SKIP LOCKED` | Submit, run, and inspect a job; demonstrate a worker crash leaving `running` stuck |
| V2 | RabbitMQ, transactional outbox, dispatcher, and manual acknowledgements | Jobs committed while RabbitMQ is unavailable publish later |
| V3 | Controlled multiple workers on the synthetic workload using local Go processes | Observe competing consumers, concurrent execution, safe duplicate claims, and continued work after one worker stops |
| V4 | Working image application: bounded uploads, thumbnails, batch tracking, downloads, and a usable frontend | One upload completes end to end; then batches of up to four images work |
| V5 | Retry classification/backoff, attempt history, leases, crash recovery, reliable redispatch, and idempotent results | Transient errors retry; abandoned work recovers without duplicate visible output |
| V6 | Finished resume-ready product: UI polish, CI, documentation, useful metrics, and a small demo | A clean checkout can reproduce the documented workflow and demonstrate its guarantees |

The durable job states begin in V1 as `queued`, `running`, `succeeded`, and `failed`. V5 introduces `retry_wait`. Cancellation and a later `cancelled` state are optional after reliability works. PostgreSQL stores application failures and attempt history; any future RabbitMQ DLQ would store delivery-system artifacts, not canonical job history.

PostgreSQL and RabbitMQ remain in Docker while the API, dispatcher, and workers run as local Go processes. Application containerization is optional when it materially improves reproducibility, deployment, or scaling. The first image workflow shares bounded local filesystem storage on the same Windows machine. Default to one worker; use two to validate concurrency.

V6 is the finished-product milestone. Further image operations, cancellation, priorities, backpressure, richer dashboards, tracing, S3/MinIO, Redis, cloud deployment, and Kubernetes are optional backlog items requiring a concrete need. No additional mandatory versions follow V6.

The V4 sequence is: one JPEG/PNG upload → validate and safely store → durable job plus outbox → existing dispatcher/RabbitMQ → Go thumbnail worker → persisted output → download. Then extend to batches of up to four images, add a minimal browser interface, and validate the complete flow with small test images. Preserve `synthetic.sha256`; add no new infrastructure. V4 implementation requires separate approval.
