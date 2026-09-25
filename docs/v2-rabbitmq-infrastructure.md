# V2.1 RabbitMQ infrastructure

V2.1 adds RabbitMQ to the local Docker Compose environment without changing the V1 job-processing path. The API still writes jobs to PostgreSQL and the worker still polls PostgreSQL. The outbox, dispatcher, RabbitMQ consumer, and acknowledgements begin in later V2 checkpoints.

## Why RabbitMQ

PostgreSQL polling repeatedly asks whether work exists, adding idle database traffic and polling latency. RabbitMQ is designed to deliver messages to waiting consumers and track whether deliveries are ready or unacknowledged. It will carry notifications that a job may need processing.

PostgreSQL remains authoritative for job status, payload, result, and application failure history. RabbitMQ does not provide an atomic commit with the PostgreSQL transaction, exactly-once execution, or canonical application lifecycle state. V2.2 introduces a transactional outbox so a committed job cannot lose its publication intent when RabbitMQ is unavailable.

## Terms and proposed topology

- An **exchange** receives publications from a producer and routes them according to its type and bindings.
- A **queue** holds routed messages until a consumer handles them.
- A **binding** connects an exchange to a queue.
- A **routing key** is the label a direct exchange compares with bindings.
- The **producer** will be the V2.3 outbox dispatcher.
- The **consumer** will be the V2.4 worker.
- A **durable queue** retains its definition across broker restarts.
- A **persistent message** asks RabbitMQ to persist that message. Durable queues and persistent messages address different parts of restart durability and will be used together.

The reserved V2 topology is deliberately small:

| Component | Name |
| --- | --- |
| Direct exchange | `cloudforge.jobs` |
| Durable queue | `cloudforge.jobs.v1` |
| Binding routing key | `job.ready` |

V2.1 does not declare this topology. The later messaging code will declare it idempotently when connecting, so the components that depend on the topology also verify it exists.

## Local service

Docker Compose runs the official `rabbitmq:4-management` image. AMQP is published on port `5672`, and the management HTTP interface is published on `15672`. A named volume stores broker data, a stable hostname keeps the RabbitMQ node identity consistent with that volume, and the healthcheck uses `rabbitmq-diagnostics ping`.

Local credentials and published ports come from `.env`. The values in `.env.example` are development defaults and must not be reused as production secrets.

Open [http://localhost:15672](http://localhost:15672) and sign in. Use:

- **Exchanges** to inspect exchanges and their bindings.
- **Queues and Streams** to inspect queues.
- A queue's page to inspect ready and unacknowledged message counts, consumers, bindings, and message rates.
- **Connections** and **Channels** to inspect producers and consumers after later V2 processes connect.

The management UI's recent statistics are transient. Durable topology and persistent messages live in the RabbitMQ data volume.
