//go:build integration

package system_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/httpapi"
	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
)

func startV4Dispatcher(t *testing.T, f *v3Fixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := dispatcher.New(f.outbox, func(ctx context.Context) (dispatcher.Publisher, error) {
		return cloudrabbit.Open(ctx, f.cfg.RabbitMQURL, f.cfg.DispatcherConfirmTimeout)
	}, 10*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond, f.logger)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("dispatcher cleanup timeout")
		}
	})
	go func() { done <- runner.Run(ctx) }()
}

func postV4Batch(t *testing.T, server *httptest.Server, data [][]byte, want int) jobs.Batch {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for index, image := range data {
		part, err := writer.CreateFormFile("images", fmt.Sprintf("image-%d.png", index+1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(image); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Post(server.URL+"/v1/batches", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("batch status=%d want=%d body=%s", res.StatusCode, want, payload)
	}
	if want != 202 {
		return jobs.Batch{}
	}
	var batch jobs.Batch
	if err := json.Unmarshal(payload, &batch); err != nil {
		t.Fatal(err)
	}
	if batch.JobCount != len(data) || batch.Queued != len(data) || res.Header.Get("Location") != "/v1/batches/"+batch.ID {
		t.Fatalf("acceptance=%s", payload)
	}
	return batch
}

func getV4Batch(t *testing.T, server *httptest.Server, id string) jobs.Batch {
	t.Helper()
	res, err := server.Client().Get(server.URL + "/v1/batches/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var batch jobs.Batch
	if res.StatusCode != 200 {
		t.Fatalf("get batch status=%d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&batch); err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestV4BatchWorkflow(t *testing.T) {
	f := newV3Fixture(t)
	root := t.TempDir()
	storage, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	server := httptest.NewServer(httpapi.NewWithImages(jobs.NewService(f.jobs), f.pool, f.logger, storage).Handler())
	t.Cleanup(server.Close)
	data := [][]byte{v4Image(t, 640, 320, "jpeg", 255), v4Image(t, 48, 24, "png", 96), v4Image(t, 240, 480, "png", 80), v4Image(t, 2, 2, "png", 255)}
	batch := postV4Batch(t, server, data, 202)
	var records, jobsCount, events int
	err = f.pool.QueryRow(context.Background(), `SELECT count(*),count(j.id),count(e.id)
        FROM cloudforge.batch_images i JOIN cloudforge.jobs j ON j.id=i.job_id
        JOIN cloudforge.outbox_events e ON e.aggregate_id=j.id WHERE i.batch_id=$1`, batch.ID).Scan(&records, &jobsCount, &events)
	if err != nil || records != 4 || jobsCount != 4 || events != 4 {
		t.Fatalf("atomic records=%d/%d/%d err=%v", records, jobsCount, events, err)
	}
	for _, job := range batch.Jobs {
		if _, err := os.Stat(filepath.Join(root, "inputs", job.InputID, "image")); err != nil {
			t.Fatal(err)
		}
		if job.OutputURL != "" {
			t.Fatal("unfinished output link")
		}
		v4Download(t, server, job.ID, 409)
	}
	// A deterministic gate exposes a real running job without burning CPU or
	// inflating images; the real processor runs immediately after release.
	started := make(chan string, 4)
	gate, release := newGate()
	worker := f.startWorker(t, "v4-worker", f.jobs, func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		select {
		case started <- job.ID:
		default:
		}
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return storage.Execute(ctx, job)
	}, release)
	f.waitForConsumers(t, 1)
	startV4Dispatcher(t, f)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not start")
	}
	active := getV4Batch(t, server, batch.ID)
	if active.Running != 1 || active.Queued != 3 || active.Succeeded != 0 || active.Failed != 0 {
		t.Fatalf("actual active states=%+v", active)
	}
	t.Log("one real running job and three queued jobs, persisted states observed through HTTP")
	release()
	for _, job := range batch.Jobs {
		f.waitForStatus(t, job.ID, jobs.StatusSucceeded)
	}
	complete := getV4Batch(t, server, batch.ID)
	if complete.Succeeded != 4 || complete.Queued != 0 || complete.Running != 0 || complete.Failed != 0 {
		t.Fatalf("counts=%+v", complete)
	}
	dims := [][3]int{{320, 160, 255}, {48, 24, 96}, {160, 320, 80}, {2, 2, 255}}
	for index, job := range complete.Jobs {
		if job.OutputURL != "/v1/jobs/"+job.ID+"/thumbnail" {
			t.Fatal("missing completed output link")
		}
		v4AssertThumbnail(t, server, job.Job, dims[index][0], dims[index][1], uint8(dims[index][2]))
	}
	single := postV4Batch(t, server, data[1:2], 202)
	f.waitForStatus(t, single.Jobs[0].ID, jobs.StatusSucceeded)
	if got := getV4Batch(t, server, single.ID); got.JobCount != 1 || got.Succeeded != 1 {
		t.Fatalf("single batch=%+v", got)
	}

	// Reject the second outbox insert after the first image's rows have been
	// registered. Every batch record and input must still roll back together.
	var beforeBatches, beforeFiles, beforeJobs, beforeEvents int
	countSQL := `SELECT (SELECT count(*) FROM cloudforge.batches),(SELECT count(*) FROM cloudforge.batch_images),(SELECT count(*) FROM cloudforge.jobs),(SELECT count(*) FROM cloudforge.outbox_events)`
	if err := f.pool.QueryRow(context.Background(), countSQL).Scan(&beforeBatches, &beforeFiles, &beforeJobs, &beforeEvents); err != nil {
		t.Fatal(err)
	}
	inputsBefore, _ := os.ReadDir(filepath.Join(root, "inputs"))
	_, err = f.pool.Exec(context.Background(), fmt.Sprintf(`CREATE FUNCTION cloudforge.test_batch_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF (SELECT count(*) FROM cloudforge.outbox_events)>=%d THEN RAISE EXCEPTION 'reject second outbox event'; END IF; RETURN NEW; END; $$;
        CREATE TRIGGER test_batch_reject BEFORE INSERT ON cloudforge.outbox_events FOR EACH ROW EXECUTE FUNCTION cloudforge.test_batch_reject();`, beforeEvents+1))
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if _, err := f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_batch_reject ON cloudforge.outbox_events; DROP FUNCTION IF EXISTS cloudforge.test_batch_reject();`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(drop)
	postV4Batch(t, server, data[:2], 500)
	drop()
	var afterBatches, afterFiles, afterJobs, afterEvents int
	if err := f.pool.QueryRow(context.Background(), countSQL).Scan(&afterBatches, &afterFiles, &afterJobs, &afterEvents); err != nil {
		t.Fatal(err)
	}
	inputsAfter, _ := os.ReadDir(filepath.Join(root, "inputs"))
	if beforeBatches != afterBatches || beforeFiles != afterFiles || beforeJobs != afterJobs || beforeEvents != afterEvents || len(inputsBefore) != len(inputsAfter) {
		t.Fatal("partial batch or file leak after second-event rejection")
	}
	postV4Batch(t, server, [][]byte{data[0], []byte("not an image")}, 415)
	inputsAfter, _ = os.ReadDir(filepath.Join(root, "inputs"))
	if len(inputsBefore) != len(inputsAfter) {
		t.Fatal("validation failure leaked input")
	}
	// Queue a metadata-valid job with a missing input to prove failure details
	// and aggregate status come from the actual worker result.
	missing, err := f.jobs.CreateBatch(context.Background(), []jobs.BatchInput{{InputID: "00000000-0000-4000-8000-000000000001", OriginalName: "missing.png", Format: "png", Bytes: 80, Width: 2, Height: 2}})
	if err != nil {
		t.Fatal(err)
	}
	f.waitForStatus(t, missing.Jobs[0].ID, jobs.StatusFailed)
	failed := getV4Batch(t, server, missing.ID)
	if failed.Failed != 1 || failed.Jobs[0].ErrorMessage == nil || failed.Jobs[0].OutputURL != "" {
		t.Fatalf("missing failure info=%+v", failed)
	}
	v4Download(t, server, failed.Jobs[0].ID, 409)
	f.waitForACKs(t, 6)
	stopWorker(t, worker)
	f.assertSettled(t)
	t.Log("one/four-image batches, six real ACKs, dimensions/alpha/downloads, second-event rollback cleanup and failure counts passed")
}
