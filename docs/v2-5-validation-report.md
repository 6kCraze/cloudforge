# V2.5 validation report

Date: 2026-09-26

## Scope and result

V2.5 validated the complete PostgreSQL transactional-outbox, confirmed RabbitMQ publication, and manual-ACK worker path. It added one cross-component integration test, reproduced dependency outages and restart behavior against real local services, corrected two bounded-backoff bugs, and documented remaining failure windows. No V3 feature was introduced.

The detailed outcome for all 20 scenarios is in the [failure matrix](v2-failure-matrix.md). The stable design and its guarantees are in [V2 final architecture](v2-final-architecture.md).

Final verification results:

| Check | Result |
|---|---|
| `go test ./...` | Pass |
| `go vet ./...` | Pass |
| `go test -tags=integration ./... -count=1 -timeout=180s` | Pass against real PostgreSQL and RabbitMQ |
| `docker run --rm -v "${PWD}:/src" -w /src golang:1.27 go test -race ./...` | Pass |
| `git diff --check` | Pass |
| Docker Compose health | PostgreSQL and RabbitMQ healthy |
| Final queue state | durable=true, Ready=0, Unacked=0, Consumers=0 |

## Automated coverage

Existing focused tests already proved most timing-sensitive behavior:

| Behavior | Test evidence |
|---|---|
| Atomic job + outbox creation and rollback | `TestStoreCreateAndGet`, `TestCreateRollsBackJobWhenOutboxInsertFails` |
| Routed, persistent, confirmed publication | `TestRealPublicationIsConfirmedRoutedPersistentAndRecorded` |
| Mandatory unroutable handling | `TestRealUnroutablePublicationLeavesEventUnpublished` |
| Dispatcher reconnect and retained outbox | `TestDispatcherReconnectsAndPublishesPendingEvent` |
| Confirmed-before-recorded duplicate window | `TestConfirmedButUnrecordedEventCanBePublishedTwice` |
| Malformed outbox head-of-line handling | `TestMalformedOldestEventDoesNotBlockLaterValidEvent` |
| Manual ACK and prefetch one | `TestRealRabbitMQDeliveryIsProcessedThenAcknowledged`, `TestPrefetchOneKeepsSecondMessageReadyUntilFirstIsAcknowledged` |
| Duplicate notification, one execution | `TestDuplicateDeliveryExecutesJobOnlyOnce` |
| Malformed/unknown delivery does not poison worker | `TestMalformedAndUnknownMessagesDoNotPoisonConsumer` |
| Success persisted before ACK crash | `TestSucceededJobRedeliveryIsAcknowledgedWithoutReexecution` |
| Crash after claim leaves running without re-execution | `TestUnackedDeliveryIsRedeliveredAndRunningJobIsNotReexecuted` |
| Worker/dispatcher capped DB-failure backoff | `TestProcessingFailuresIncreaseBackoffAcrossSuccessfulBrokerReconnects`, `TestDatabaseFailuresIncreaseBackoffAcrossSuccessfulBrokerReconnects` |
| Cross-component dispatcher/worker downtime | `TestV2EndToEndAcrossDispatcherAndWorkerDowntime` |

The new system test creates a real PostgreSQL job/outbox event with no dispatcher, proves RabbitMQ is empty, dispatches it with no worker, proves Ready=1 while the job remains queued, then starts the real worker and proves `succeeded`, attempt 1, and a drained queue.

## Bugs discovered and corrected

Both long-running processes reset their reconnect backoff immediately after a successful RabbitMQ connection. If PostgreSQL remained down, RabbitMQ was healthy and that connection reset caused a repeated short reconnect/NACK loop rather than increasing delay.

The correction is deliberately small:

- The worker resets its backoff only after at least one delivery is handled successfully through the broker/database path.
- The dispatcher resets its backoff only after a successful outbox database read/publication cycle.
- New unit tests require approximately 20 ms, 40 ms, and 80 ms increasing retry gaps under repeated database failure.

The live worker retest showed 250 ms, 500 ms, 1 s, 2 s, 4 s, then the configured 5 s cap. The requeued message stayed Ready between attempts and completed after PostgreSQL returned. The dispatcher showed the same capped progression and recovered. This fixes current V2 retry behavior without adding retry queues, job retries, or a new architecture.

## Manual outage and restart evidence

### RabbitMQ unavailable at submission

With PostgreSQL and API running and RabbitMQ stopped, the API returned `202` for job `ea2b6ad6…`. SQL showed `queued`, attempt 0, and `published_at = NULL`. After RabbitMQ returned, the dispatcher confirmed and marked the event, the worker completed the job, and SQL showed `succeeded`, attempt 1. No publication intent was lost.

### Dispatcher absent

With the dispatcher stopped, job `fc61feb7…` remained queued with an unpublished event and no new RabbitMQ message. Starting the dispatcher published the retained event. Replaying an already-processed event later produced a `terminal duplicate skipped` log and did not increment the job attempt.

### PostgreSQL unavailable

The API returned HTTP 500 and no fake success. A worker delivery was NACKed/requeued and remained Ready while retry delays increased to the cap. After PostgreSQL returned, the worker processed it as `succeeded`, attempt 1. The dispatcher likewise backed off and resumed. Processes already running recovered; processes launched while the database was unavailable exited during their required initial connection and need operator/supervisor restart.

### RabbitMQ restart

With no worker, job `f6ecc98e…` was queued in PostgreSQL and its persistent notification was Ready. After `docker compose restart rabbitmq`, RabbitMQ reported durable queue `cloudforge.jobs.v1`, Ready=1, and the `cloudforge.jobs` / `job.ready` binding. Starting a worker completed the job and drained the queue.

### Full system restart

Two distinct durable states were prepared:

- job `8800bc27…`: queued, outbox marked published, RabbitMQ Ready=1;
- job `1a0d4904…`: queued, outbox unpublished, no RabbitMQ copy.

The API, dispatcher, worker, PostgreSQL, and RabbitMQ were stopped without deleting volumes. After PostgreSQL and RabbitMQ restarted, SQL retained both rows and their different `published_at` states, and RabbitMQ retained Ready=1. The restarted dispatcher published the missing event; the worker completed both. Final SQL showed `succeeded`, attempt 1 for each; RabbitMQ showed Ready=0 and Unacked=0.

## Startup order observations

- **RabbitMQ starts last:** API can accept through PostgreSQL. Dispatcher and worker keep retrying with capped backoff, redeclare topology, and recover automatically.
- **Infrastructure starts first:** dispatcher and worker connect normally; API order does not affect already committed work.
- **PostgreSQL returns after an outage:** already-running dispatcher and worker recover automatically with the corrected backoff; the running API handles later requests successfully.
- **Application process starts while PostgreSQL is absent:** its required initial `database.Open` fails and the process exits. An operator or supervisor must restart it. V2 does not claim otherwise.

## Repeatable validation sequence

From the repository root:

```powershell
docker compose up -d --wait db rabbitmq
. .\scripts\load-env.ps1
go run ./cmd/migrate up
go test ./...
go vet ./...
. .\scripts\load-test-env.ps1
go test -p 1 -tags=integration ./... -count=1 -timeout=180s
docker run --rm -v "${PWD}:/src" -w /src golang:1.27 go test -race ./...
git diff --check
docker compose ps
docker exec cloudforge-rabbitmq-1 rabbitmqctl list_queues name durable messages_ready messages_unacknowledged consumers
```

Current integration fixtures require the separate test database, vhost, and restricted accounts described in [integration-test setup](integration-testing.md). They never fall back to development configuration. Cleanup truncates/purges only the test namespace; do not run application processes in that namespace during tests. The table above records historical V2 results.

## Short portfolio demonstration

1. Start PostgreSQL and RabbitMQ and open [RabbitMQ Management](http://localhost:15672).
2. Start API, dispatcher, and worker in three terminals; show Consumers=1 and Ready/Unacked=0.
3. Submit a 50,000,000-iteration SHA job from the README command.
4. Query the joined job/outbox row to show atomic job intent and `published_at`.
5. In the UI, show `job.ready` routed through `cloudforge.jobs` to `cloudforge.jobs.v1`.
6. During execution, show Unacked=1 and PostgreSQL `running`; afterward show `succeeded`, attempt 1, then Ready/Unacked=0.
7. Stop the worker, submit another job, and show Ready=1, Consumers=0, job still queued. Restart worker and show it drain.
8. Simulate the confirmed-before-recorded window for a dedicated demo event by setting only its `published_at` to NULL; restart dispatcher and show a duplicate is skipped with attempt still 1.
9. Stop RabbitMQ, submit a job, and show `202`, queued row, and NULL `published_at`; restore RabbitMQ and show automatic completion.
10. Finish with the crash limitation: force-kill a worker after a long job reaches `running`, restart it, and show RabbitMQ redelivery is ACKed while PostgreSQL remains visibly `running`.

The management graphs sample periodically, so the long workload is necessary to make Unacked visible. The crash demo intentionally leaves a stuck row and should use a disposable job.

## Remaining architecture debt before later phases

- Abandoned `running` jobs need leases, heartbeats, and a recovery policy before CloudForge can promise automatic crash recovery.
- Application and message failures have logs but no retry schedule, DLQ/quarantine, or persisted rejection history.
- One dispatcher is supported; scaling it requires coordination/claims.
- Startup depends on an external process supervisor when PostgreSQL is absent at launch.
- Retained published outbox rows need an archival/retention policy for long-lived operation.
- API submission has no idempotency key for a client that loses a successful response.

These are explicit boundaries for future work, not hidden V2 regressions.
