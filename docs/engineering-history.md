# Engineering milestones

The [README](../README.md) describes the current product; [architecture](architecture.md) describes its implemented reliability boundaries. These milestones preserve how the design evolved.

| Phase | Concern introduced | Design and evidence |
| --- | --- | --- |
| V0 | Local PostgreSQL, configuration, migration tooling | [Roadmap](roadmap.md), [development setup](development.md) |
| V1 | Synthetic SHA-256 jobs, polling worker, `running`, `SKIP LOCKED` | [Original architecture and deliberate stuck-running crash](v1-architecture.md) |
| V2.1 | Durable local broker and management interface | [RabbitMQ infrastructure](v2-rabbitmq-infrastructure.md) |
| V2.2 | Atomic job and publication intent | [Transactional outbox](v2-2-transactional-outbox.md) |
| V2.3 | Sequential confirmed, mandatory publication | [Dispatcher and duplicate window](v2-3-outbox-dispatcher.md) |
| V2.4 | Manual ACK, prefetch, duplicate handling | [Worker consumption](v2-4-rabbitmq-worker.md) |
| V2.5 | Broker/database/process failure behavior | [Final V2 architecture](v2-final-architecture.md), [failure matrix](v2-failure-matrix.md), [validation](v2-5-validation-report.md) |
| V3 | Two competing workers and deterministic concurrent claims | [Scaling proof](v3-multiple-workers.md) |
| V4 | Real thumbnail processing, bounded uploads and batch browser UI | [Image workflow](v4-image-workflow.md) |
| V5 | Leased ownership, attempt fencing, recovery and bounded retries | [Ownership](v5-safe-ownership.md), [fault validation](v5-recovery.md) |
| V6 | Interactive system experience, safe local launcher, CI, portfolio | [Experience](v6-system-experience.md), [tooling](v6-engineering-validation.md), [portfolio](v6-portfolio-validation.md) |

## The failure that motivated recovery

V1 deliberately left a job stuck in `running` when its worker crashed. V2 RabbitMQ redelivery alone did not repair that PostgreSQL state: a replacement worker refused to claim an already running job. The historical `TestUnackedDeliveryIsRedeliveredAndRunningJobIsNotReexecuted` demonstrated this limitation. V5 adds the database lease/recovery/outbox mechanism that resolves it. These older reports describe the system at their version, not current guarantees.

Do not repeat the historical 50-million-iteration crash demo for routine smoke testing. Current deterministic integration cases exercise ownership and recovery using small or blocking processors. Graceful Ctrl+C is different from a forced crash: intake stops and the current delivery is allowed to reach a durable decision.

## Synthetic compatibility

With the normal application running:

```powershell
$body = @{ type = 'synthetic.sha256'; payload = @{ text = 'hello CloudForge'; iterations = 1000 } } | ConvertTo-Json
$job = Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/jobs -ContentType application/json -Body $body
Invoke-RestMethod "http://localhost:8080/v1/jobs/$($job.id)"
```

See the [ADRs](adr/) for the original technology and messaging decisions. The current final release target is V6; no additional mandatory milestones follow it.
