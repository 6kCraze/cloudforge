# V4: image batches and browser application

The browser and single-image workflows use the existing PostgreSQL → outbox → dispatcher → RabbitMQ → manual-ACK Go worker flow. The API stores and validates the upload; it does not generate the thumbnail. PostgreSQL remains authoritative, and RabbitMQ messages still contain only job IDs. Existing `synthetic.sha256` submissions and processing are preserved.

V3 checkpoint: `origin/main` and annotated tag `v0.3.0` resolve to `6d70057f9b65e280b3f998b74698e504324d4c0c`. V4 changes are uncommitted.

## API

| Route | Behavior |
|---|---|
| `GET /` | Embedded responsive browser application; no frontend server or build |
| `POST /v1/batches` | Multipart upload with one to four file parts named `images`; returns `202` with the durable batch and queued jobs |
| `GET /v1/batches/{id}` | Batch metadata, authoritative counts, ordered individual jobs, failure information and completed output links |
| `POST /v1/images` | Raw body containing one JPEG/PNG; returns `202` with `{id,status}` and a job Location header after the job/outbox commit |
| `GET /v1/jobs/{id}` | Existing authoritative job status, payload, result, or execution error |
| `GET /v1/jobs/{id}/thumbnail` | Downloads the completed PNG after checking job type, succeeded state, ownership, size, dimensions, and full PNG integrity |
| `POST /v1/jobs` | Existing SHA-256 JSON contract; clients cannot use it to submit arbitrary stored file references |

The single-image endpoint accepts a raw image body. Batches use multipart form data with a **21 MiB total request boundary**, including unknown-length bodies and trailing multipart epilogues. Each part reuses the existing streaming storage/validation path, and only small metadata is retained for the transaction; the whole batch is never buffered or pixel-decoded together. The filename and declared MIME type are irrelevant to validation; JPEG/PNG decoders inspect actual bytes. Limits are 5 MiB per file, 8,000,000 decoded pixels, and 8,192 pixels per dimension. Header dimensions are checked before full decoding; full validation rejects truncated/corrupt input. The worker repeats these checks rather than trusting stored files blindly.

Batch responses contain `id`, `created_at`, `job_count`, `queued`, `running`, `succeeded`, `failed`, and `jobs`. Each member has its job ID/state, validated input metadata, display name, execution error when available, and `output_url` after success. Counts come from one PostgreSQL statement snapshot; there are no stored counters or elapsed-time percentages. Display names are bounded text metadata and never become filesystem paths or HTML markup.

Structured upload errors include `413 image_too_large`, `422 image_dimensions`, `415 unsupported_image`, `400 invalid_image`, and `507 storage_full`. Queued, running, and failed image jobs return `409 job_not_succeeded` for downloads. Missing jobs/files return `404`; inconsistent or incomplete successful results return `409`. SHA jobs have no thumbnail endpoint result.

## Storage and processing

Set an absolute `STORAGE_ROOT` in the environment shared by the API and worker. When blank, both use the current user's cache directory followed by `CloudForge/storage`; on Windows this is normally `%LOCALAPPDATA%\CloudForge\storage`. Use a dedicated directory, and run both local processes as the same user or explicitly configure the same root.

```text
STORAGE_ROOT/
  staging/<generated UUID>.tmp
  inputs/<generated input UUID>/image
  outputs/<job UUID>/thumbnail.png
```

Storage uses Go's confined `os.Root` filesystem operations and canonical UUID identifiers. Client strings never become paths. Temporary files are created exclusively; completed files are synced and closed, then renamed into an exclusively reserved destination directory in the same root. Existing output directories are refused rather than overwritten. Staging files are removed on normal failure.

One API upload is decoded at a time. Admission uses a conservative 256 MiB budget for the dedicated root, including a reserved 512 KiB output allowance for every retained input. Worker output is bounded to 512 KiB. This is application admission control for one API process per root, not an operating-system disk quota; external writes and multiple API processes are outside this first slice. Published input/output files are retained; there is no automatic retention policy yet.

The input is finalized **before** the database transaction begins. Migration `00004_add_image_thumbnail.sql` expands the existing job-type constraint; it adds no new table or lifecycle state. The image job payload contains `input_id`, and the same transaction inserts its `job.ready` event. A known transaction failure removes the just-uploaded input. A lost/uncertain commit response retains it and logs the input/job IDs for later reconciliation. Filesystem writes and PostgreSQL commits are not one atomic operation.

Migration `00005_add_image_batches.sql` adds `batches` and `batch_images`. Batch metadata, validated input metadata, one job per image, and one outbox event per job share **one transaction**, after every input file is finalized. A failure on a later insert rolls back all earlier rows and cleans up all known-unreferenced inputs. An uncertain commit preserves every input and logs the batch/input IDs. Rolling back migration five removes batch metadata but preserves job/outbox history; it must be intentional.

The worker supports both SHA-256 and `image.thumbnail`. It scales proportionally into 320×320, keeps smaller images at their original dimensions, and preserves alpha in PNG output. Scaling uses the small [Go x/image/draw package](https://pkg.go.dev/golang.org/x/image/draw). Integer dimensions can round by less than one pixel. Output is finalized before the existing worker records `succeeded` and ACKs. The result contains `output_id` equal to the job UUID, width, height, format `png`, and byte count; no absolute filesystem path is exposed.

## Run on Windows

The local development database was upgraded to migration five on October 26, 2026 after explicit approval. The pre-upgrade archive's checksum and `pg_restore --list` were verified; a full restore was not tested. For any other database, inspect its target, schema version, and existing data and take a backup before applying pending migrations from the repository root. Automated integration validation uses only the isolated test database:

```powershell
docker compose up -d --wait --pull never db rabbitmq
. .\scripts\load-env.ps1
go run ./cmd/migrate up
```

In three separate terminals, load the same `.env`, then run `go run ./cmd/api`, `go run ./cmd/dispatcher`, and `go run ./cmd/worker` respectively. Use only one worker for ordinary development. A configured absolute `STORAGE_ROOT` must match in the API and worker terminals; leaving it blank uses the shared per-user default.

Open **http://localhost:8080/**. Choose or drag in one to four images, review previews, and click **Process images**. Finished results have previews and **Download PNG** buttons. The batch link restores saved status after reload. Polling waits two seconds between active updates, stops at terminal states, and supports pause/resume. If the API becomes unavailable, polling pauses with a readable message instead of resubmitting uploads or starting duplicate loops.

The existing single-image route remains available from a fourth terminal:

```powershell
$accepted = Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/v1/images' `
  -ContentType 'application/octet-stream' -InFile 'C:\Images\photo.png'
$accepted
Invoke-RestMethod "http://localhost:8080/v1/jobs/$($accepted.id)" | ConvertTo-Json -Depth 5
# Once the job says succeeded:
Invoke-WebRequest "http://localhost:8080/v1/jobs/$($accepted.id)/thumbnail" -OutFile "$env:TEMP\thumbnail.png"
```

The interface is plain HTML/CSS/JavaScript embedded in the API binary. Stop the three application terminals when finished and use `docker compose stop db rabbitmq` to stop infrastructure without removing volumes.

## Validation

Use the [isolated test setup](integration-testing.md). Test images are small, with dimension violations simulated using small PNG headers before pixel decoding. Filesystem integration artifacts use `t.TempDir`, not development storage. Initial isolated validation migrated only the test database; the approved development migration and browser demonstration followed on October 26, 2026.

Executed commands:

```powershell
go test -p 1 ./...
. .\scripts\load-test-env.ps1
go test -p 1 -tags=integration ./internal/jobs `
  -run '^(TestStoreCreateAndGet|TestCreateRollsBackJobWhenOutboxInsertFails)$' -count=1 -v -timeout=60s
go test -p 1 -tags=integration ./internal/system `
  -run '^TestV4SingleImageWorkflow$' -count=1 -v -timeout=60s
go test -p 1 -tags=integration ./internal/system `
  -run '^TestV4BatchWorkflow$' -count=1 -v -timeout=60s
go vet -p 1 ./...
go vet -p 1 -tags=integration ./...
git diff --check
```

The end-to-end test uploads JPEG and PNG through real HTTP handlers. It first verifies the stored input, queued job, and unpublished outbox intent with the dispatcher stopped. It then starts the real dispatcher and one real broker consumer, checks downloaded dimensions/aspect/alpha, and verifies a SHA digest through the same worker. A test-only outbox rejection proves both row rollback and input cleanup; missing input proves persisted execution failure and denied download. Four actual ACK calls and an empty broker queue after consumer closure are observed. V3 concurrency tests and the four-job SHA demonstration are not rerun.

Unit tests cover malformed/unsupported/oversized inputs, small-header dimension violations, worker revalidation, no-upscale behavior, transparency, exclusive output publication, path guards, incomplete/corrupt output denial, and commit-uncertainty retention. Commit reply classification covers explicit rollback, constraint/serialization failures, lost sockets, cancellation, connection errors, and SQLSTATE completion-unknown. The upload retention test injects an uncertain result; it does not simulate an actual network failure at COMMIT.

All commands above passed. Targeted race tests also passed for `internal/images`, `internal/httpapi`, and `TestV4SingleImageWorkflow` in `internal/system`, using the already cached Go 1.27 Linux image with no pull, one CPU, a 2 GiB memory cap, `GOMAXPROCS=2`, and package parallelism one. No stress workload was used.

Development jobs and outbox snapshots before/after isolated validation have the identical SHA-256 hash `15D0B030483F6A0A7C03B777ADDCCDA27A718C4326B42F1E7FDA8E17D38F18DD`. All four V3 demonstration rows remained succeeded with attempt count one and their original worker assignments and timing. At that point development migration version was three; only the test database had received migrations four and five. Infrastructure was stopped after validation, preserving its volumes.

## Batch and browser validation

`TestV4BatchWorkflow` passed with **one real RabbitMQ worker**: one/four-image batches, four consistent metadata/job/outbox records, deterministic observation of one running and three queued jobs, correct dimensions/alpha and downloads, second-outbox-insert rollback with file cleanup, and a real missing-input failure with useful errors. Six actual ACK calls and an empty queue after worker shutdown were observed. Boundary unit tests reject a fifth image, an invalid later image, per-image oversize, declared/unknown-length request oversize, and truncated multipart without registering a partial batch or leaving inputs. An injected uncertain commit retains all batch inputs. Migration five also passed a down/up round trip on the isolated test database.

The opt-in `TestV4BrowserSession` served the production HTTP handlers at `127.0.0.1:18080` against the isolated test database/vhost, temporary storage, one real dispatcher, and one real worker. Browser automation actually selected, submitted, previewed and downloaded **one-image and four-image batches**, verified all four loaded previews, and reloaded a saved batch. The downloaded files were decoded separately: JPEG result **320×160**; transparent PNG result **160×320**, alpha **96**. A file disguised as PNG produced the actual server error in the UI. Mobile width 390 had no horizontal overflow. Chrome extension upload permission was unavailable, so the complete workflow ran in the Codex in-app browser. The temporary runtime was stopped and its test-owned files/records cleaned up.

The existing five integration fixture files now include batch tables in their guarded test-database truncation, as required by the new foreign key. Isolation checks and advisory locks remain unchanged. Production dispatcher, competing consumers, prefetch one, atomic claiming, and manual ACK logic are unchanged. No V3 concurrency suite or demonstration was rerun.

Final ordinary tests, targeted transaction/single-image compatibility tests, and `go vet -p 1 -tags=integration ./...` passed. Batch request/cleanup tests, browser asset tests, commit-outcome tests, and `TestV4BatchWorkflow` passed under the race detector in the cached Go 1.27 container, with one CPU, 2 GiB cap, `GOMAXPROCS=2`, package parallelism one, no pull and offline module access. Validation ran sequentially.

During the browser session, observed resident memory was approximately 62 MiB for the API/dispatcher/worker test process, 33 MiB for its Go launcher, 61 MiB for PostgreSQL and 173 MiB for RabbitMQ; these are observations rather than peak measurements or total Docker/browser memory. No application containerization, image builds, dependency additions, stress tests or large pixel fixtures were introduced in the batch continuation. All temporary Go processes and the race container exited. Both broker queues had zero ready/unacknowledged messages and consumers, then PostgreSQL and RabbitMQ were stopped. Their volumes and development records were preserved.

## Files in this implementation

38 files total, including the approved single-image slice.

Modified: `.env.example`, `.gitignore`, `README.md`, `go.mod`, `go.sum`, `cmd/api/main.go`, `cmd/worker/main.go`, `internal/config/config.go`, `internal/config/config_test.go`, `internal/httpapi/api.go`, `internal/jobs/model.go`, `internal/jobs/service.go`, `internal/jobs/store.go`, `internal/jobs/store_integration_test.go`, `internal/dispatcher/dispatcher_integration_test.go`, `internal/worker/worker_integration_test.go`, `internal/system/v2_integration_test.go`, and `internal/system/v3_multi_worker_integration_test.go`.

Created: `migrations/00004_add_image_thumbnail.sql`, `migrations/00005_add_image_batches.sql`, `internal/images/storage.go`, `internal/images/processor.go`, `internal/images/images_test.go`, `internal/httpapi/images.go`, `internal/httpapi/images_test.go`, `internal/httpapi/batches.go`, `internal/httpapi/batches_test.go`, `internal/httpapi/web.go`, `internal/httpapi/web/index.html`, `internal/httpapi/web/app.css`, `internal/httpapi/web/app.js`, `internal/httpapi/web/icon.svg`, `internal/jobs/batches.go`, `internal/jobs/commit_test.go`, `internal/system/v4_images_integration_test.go`, `internal/system/v4_batches_integration_test.go`, `internal/system/v4_browser_integration_test.go`, and this document. No credentials, generated images, binaries, or database snapshots are included.

## Limits and next step

Forced crashes can still leave running jobs stuck, or leave files needing reconciliation. An output finalized before a failed terminal database write is not downloadable until authoritative success exists. Output replacement/recovery, leases, retries, fencing, and reliable redispatch remain V5 work. EXIF orientation is not applied in this first thumbnail slice.

The approved local development migration and browser demonstration also passed on October 26, 2026. Schema version five is applied; the original four V3 jobs and four outbox rows matched their pre-migration hashes after the demo. A single JPEG upload and a four-PNG batch completed through the real dispatcher, RabbitMQ, and one worker. All five previews loaded and all five PNGs downloaded and decoded. A saved batch remained complete after browser reload, and a small `synthetic.sha256` job succeeded. The queue ended with zero ready and unacknowledged messages. Temporary Go processes and both infrastructure containers were stopped; the persistent volumes, jobs, image files, and downloaded files were preserved. V4 is ready for release review; no commit, push, or tag was performed. V5 should address leases, retries, crash recovery, output reconciliation/idempotency, and failure testing. No cloud storage, authentication, extra processing formats, or additional infrastructure was added here.
