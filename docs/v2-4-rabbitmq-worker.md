# V2.4 RabbitMQ worker

## Architecture

```text
API -- one PostgreSQL transaction --> jobs + outbox_events
                                             |
                                        dispatcher
                                             |
                                      RabbitMQ queue
                                             |
                                  manual-ACK Go worker
                                             |
                           load, claim, and finish job in PostgreSQL
```

RabbitMQ delivers a notification that work may be available. PostgreSQL remains the authority for the job payload, owner, attempt count, status, result, and application failure. The production worker no longer searches PostgreSQL for queued jobs. The historical `ClaimNext` implementation and its `FOR UPDATE SKIP LOCKED` tests remain as V1 learning material, while V2.4 uses `ClaimByID` for the job named by the message.

## Consumer and acknowledgement flow

The worker idempotently declares the durable V2 topology, sets channel QoS to `prefetch=1`, and consumes `cloudforge.jobs.v1` with `autoAck=false`. Prefetch one lets RabbitMQ send this worker only one unacknowledged delivery at a time. This favors visible, simple failure semantics over throughput.

With auto-ack, RabbitMQ could remove a delivery before CloudForge finishes it. A crash would then lose the delivery. V2.4 instead keeps it unacknowledged while the worker loads and handles the job:

```text
receive -> validate -> load PostgreSQL job -> claim queued job
        -> execute -> persist succeeded/failed -> ACK
```

If the process or connection dies before ACK, RabbitMQ returns the delivery to the ready state and can redeliver it. Redelivery does not itself authorize execution; the consumer always checks PostgreSQL again.

## ACK and NACK policy

| Delivery or PostgreSQL state | V2.4 action | Broker result |
| --- | --- | --- |
| Valid message, job `queued` | Atomically claim by ID, execute, persist terminal state | ACK after persistence |
| Job `succeeded` | Skip duplicate | ACK |
| Job `failed` | Treat as terminal | ACK |
| Job `running` | Do not guess whether its owner is alive; leave it running | ACK |
| Unknown job UUID | Log and drop; current foreign-key-backed outbox design cannot make it valid later | ACK |
| Invalid content type, type, message ID, JSON, or job UUID | Log and drop to prevent a poison-message loop | ACK |
| PostgreSQL read, claim, or terminal-write error | Do not claim successful handling | NACK with requeue, then reconnect after backoff |
| Processor error successfully persisted as `failed` | Application outcome is durable | ACK |

There is no DLQ in V2.4. ACKing malformed and unknown deliveries is a deliberate temporary drop policy: requeueing an unchanged invalid message would create an infinite hot loop. The structured error log is the evidence available in this phase.

## Atomic claim and duplicates

`ClaimByID` uses one conditional statement:

```sql
UPDATE cloudforge.jobs
SET status = 'running', attempt_count = attempt_count + 1, ...
WHERE id = $1 AND status = 'queued'
RETURNING ...;
```

Only one competing delivery can change a given job from `queued` to `running`. A loser rereads PostgreSQL and applies the status policy. A later duplicate sees `succeeded` or `failed`, performs no SHA-256 work, and is ACKed. The outbox event UUID remains the RabbitMQ `message_id`, but job status is the deduplication authority.

## Running-state crash limitation

If a worker claims a job, crashes before persisting a terminal result, and never ACKs, RabbitMQ redelivers the message. PostgreSQL still says `running`. V2.4 cannot tell whether the original worker is alive or abandoned because leases and heartbeats do not exist. The replacement therefore does not re-execute the job and ACKs the redelivery, leaving the job visibly stuck in `running`.

This is conservative: it prevents concurrent duplicate execution but sacrifices automatic recovery. A later lease/recovery phase will solve the observed stuck job. V2.4 deliberately does not hide it.

One narrower failure window also remains: if execution finishes but PostgreSQL cannot record `succeeded` or `failed`, the worker NACKs and reconnects, but the job is already `running`; the conservative redelivery rule can therefore leave it stuck. Recovery requires the same later lease design.

## Reconnection and shutdown

Connection or channel failure closes the current consumer. The worker reconnects with capped exponential backoff from `WORKER_BACKOFF_MIN` to `WORKER_BACKOFF_MAX`. Every connection opens a new channel, redeclares matching topology, restores prefetch one, and starts a new manual-ACK consumer.

On Ctrl+C, the worker cancels the RabbitMQ consumer so the broker stops sending new deliveries. If one delivery is already being handled, it gets an uncancelled completion context: the worker finishes its reasonable in-progress work, persists the terminal state, ACKs, and then closes the channel, connection, and database pool. A forced process kill is different and produces RabbitMQ redelivery.

## Main manual demo

Start infrastructure and migrate from the repository root:

```powershell
docker compose up -d --wait db rabbitmq
. .\scripts\load-env.ps1
go run ./cmd/migrate up
```

Open three more PowerShell terminals in the repository root, dot-source the environment in each, and run:

```powershell
go run ./cmd/api
```

```powershell
go run ./cmd/dispatcher
```

```powershell
go run ./cmd/worker
```

Submit and inspect a job from another terminal:

```powershell
. .\scripts\load-env.ps1
$body = @{
  type = 'synthetic.sha256'
  payload = @{ text = 'hello CloudForge'; iterations = 50000000 }
} | ConvertTo-Json

$job = Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/jobs `
  -ContentType 'application/json' -Body $body
$job
Invoke-RestMethod -Uri "http://localhost:8080/v1/jobs/$($job.id)" |
  ConvertTo-Json -Depth 5
```

## RabbitMQ UI

Open [http://localhost:15672](http://localhost:15672), sign in with the local credentials from `.env`, select **Queues and Streams**, then `cloudforge.jobs.v1`.

- Worker stopped: **Consumers = 0** and published work remains **Ready**.
- Worker connected: **Consumers = 1**.
- During the long SHA job: **Unacked = 1**, because manual ACK has not happened.
- After the durable PostgreSQL result and ACK: **Ready = 0** and **Unacked = 0**.

The graphs update on a sampling interval, so very short jobs can move through Ready and Unacked too quickly to see. Use the 50,000,000-iteration workload for observation. Avoid **Get messages** because it changes queue state.

## PostgreSQL inspection

```powershell
docker exec cloudforge-db-1 psql -U cloudforge -d cloudforge -c @"
SELECT j.id, j.status, j.attempt_count, j.claimed_by,
       j.started_at, j.completed_at,
       e.id AS event_id, e.published_at
FROM cloudforge.jobs AS j
JOIN cloudforge.outbox_events AS e ON e.aggregate_id = j.id
ORDER BY j.created_at DESC
LIMIT 10;
"@
```

The job row is lifecycle truth. The outbox row records publication intent/history. RabbitMQ Ready and Unacked are delivery state, not job status.

## Duplicate and crash demonstrations

The integration test `TestDuplicateDeliveryExecutesJobOnlyOnce` publishes the same outbox event twice through real RabbitMQ and proves `attempt_count` remains one. `TestUnackedDeliveryIsRedeliveredAndRunningJobIsNotReexecuted` closes a real consumer without ACK after claiming, proves RabbitMQ marks the next delivery as redelivered, and proves the running job is not executed again.

For a visible crash demo, build and start the worker executable, submit a long job, wait until PostgreSQL says `running`, and force-kill it:

```powershell
. .\scripts\load-env.ps1
go build -o "$env:TEMP\cloudforge-worker.exe" ./cmd/worker
$worker = Start-Process "$env:TEMP\cloudforge-worker.exe" -PassThru -NoNewWindow
# Submit the 50,000,000-iteration job shown above, then while it is running:
Stop-Process -Id $worker.Id -Force
```

The RabbitMQ queue returns the unacknowledged delivery to Ready. Restart the worker executable. The delivery disappears after the replacement sees authoritative `running` state and ACKs it, while PostgreSQL deliberately remains `running`.

## Known V2.4 limitations

- There are no leases, heartbeats, or abandoned-running recovery.
- There are no job retries, retry queues, DLQ, or persisted message-rejection history.
- Malformed and unknown deliveries are only logged before being dropped.
- Terminal-state write failure after a successful claim can leave a running job stuck.
- Graceful shutdown waits for current synthetic work and has no configurable drain deadline.
- Only one worker is an intended V2.4 operating mode; multi-worker scaling is V3.
