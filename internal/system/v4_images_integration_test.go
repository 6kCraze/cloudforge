//go:build integration

package system_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
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

func TestV4SingleImageWorkflow(t *testing.T) {
	// Reuse the already guarded isolated infrastructure fixture, not the V3
	// concurrency scenarios. All files live in a test-owned temporary directory.
	f := newV3Fixture(t)
	root := t.TempDir()
	apiFiles, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = apiFiles.Close() })
	workerFiles, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workerFiles.Close() })
	api := httpapi.NewWithImages(jobs.NewService(f.jobs), f.pool, f.logger, apiFiles)
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	upload := func(data []byte, want int) jobs.Job {
		t.Helper()
		req, err := http.NewRequest("POST", server.URL+"/v1/images", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately incorrect MIME type; actual image contents decide validity.
		req.Header.Set("Content-Type", "application/octet-stream")
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("upload status=%d want %d body=%s", res.StatusCode, want, body)
		}
		if want != http.StatusAccepted {
			return jobs.Job{}
		}
		var accepted struct{ ID, Status string }
		if err := json.Unmarshal(body, &accepted); err != nil {
			t.Fatal(err)
		}
		if accepted.Status != jobs.StatusQueued || res.Header.Get("Location") != "/v1/jobs/"+accepted.ID {
			t.Fatalf("acceptance=%s", body)
		}
		return f.getJob(t, accepted.ID)
	}

	jpegJob := upload(v4Image(t, 640, 320, "jpeg", 255), 202)
	var payload jobs.ThumbnailPayload
	if err := json.Unmarshal(jpegJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "inputs", payload.InputID, "image")); err != nil {
		t.Fatal("queued job references missing input", err)
	}
	var eventCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM cloudforge.outbox_events WHERE aggregate_id=$1 AND published_at IS NULL`, jpegJob.ID).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("durable unpublished intent count=%d err=%v", eventCount, err)
	}
	v4Download(t, server, jpegJob.ID, 409)
	t.Log("JPEG fully stored before durable queued job plus unpublished outbox; queued download blocked")

	worker := f.startWorker(t, "v4-worker", f.jobs, workerFiles.Execute, func() {})
	f.waitForConsumers(t, 1)
	dispatcherCtx, cancelDispatcher := context.WithCancel(context.Background())
	dispatcherDone := make(chan error, 1)
	runner := dispatcher.New(f.outbox, func(ctx context.Context) (dispatcher.Publisher, error) {
		return cloudrabbit.Open(ctx, f.cfg.RabbitMQURL, f.cfg.DispatcherConfirmTimeout)
	}, 10*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond, f.logger)
	t.Cleanup(func() {
		cancelDispatcher()
		select {
		case err := <-dispatcherDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("dispatcher cleanup timed out")
		}
	})
	go func() { dispatcherDone <- runner.Run(dispatcherCtx) }()

	jpegDone := f.waitForStatus(t, jpegJob.ID, jobs.StatusSucceeded)
	v4AssertThumbnail(t, server, jpegDone, 320, 160, 255)
	pngJob := upload(v4Image(t, 48, 24, "png", 96), 202)
	pngDone := f.waitForStatus(t, pngJob.ID, jobs.StatusSucceeded)
	v4AssertThumbnail(t, server, pngDone, 48, 24, 96)
	t.Log("JPEG and PNG traveled through real dispatcher/broker/one Go worker; aspect ratio, alpha and no-upscale verified")

	shaRequest := `{"type":"synthetic.sha256","payload":{"text":"V4 SHA compatibility","iterations":1}}`
	res, err := server.Client().Post(server.URL+"/v1/jobs", "application/json", bytes.NewBufferString(shaRequest))
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct{ ID string }
	err = json.NewDecoder(res.Body).Decode(&accepted)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != 202 {
		t.Fatalf("SHA submission status=%d err=%v", res.StatusCode, err)
	}
	shaDone := f.waitForStatus(t, accepted.ID, jobs.StatusSucceeded)
	var shaResult jobs.SHA256Result
	if err := json.Unmarshal(shaDone.Result, &shaResult); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256([]byte("V4 SHA compatibility"))
	if shaResult.Digest != hex.EncodeToString(wantDigest[:]) || shaDone.AttemptCount != 1 {
		t.Fatalf("SHA compatibility result=%+v", shaResult)
	}

	// Ordinary DB failure after file finalization must roll back both rows and
	// remove only that upload. Trigger exists exclusively in the test database.
	beforeInputs, _ := os.ReadDir(filepath.Join(root, "inputs"))
	var beforeJobs, beforeEvents int
	if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cloudforge.jobs),(SELECT count(*) FROM cloudforge.outbox_events)`).Scan(&beforeJobs, &beforeEvents); err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(context.Background(), `CREATE FUNCTION cloudforge.test_v4_reject_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'V4 test rejected outbox'; END; $$;
	CREATE TRIGGER test_v4_reject_outbox BEFORE INSERT ON cloudforge.outbox_events FOR EACH ROW EXECUTE FUNCTION cloudforge.test_v4_reject_outbox();`)
	if err != nil {
		t.Fatal(err)
	}
	dropTrigger := func() {
		_, err := f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_v4_reject_outbox ON cloudforge.outbox_events; DROP FUNCTION IF EXISTS cloudforge.test_v4_reject_outbox();`)
		if err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(dropTrigger)
	upload(v4Image(t, 2, 2, "png", 255), 500)
	dropTrigger()
	var afterJobs, afterEvents int
	if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cloudforge.jobs),(SELECT count(*) FROM cloudforge.outbox_events)`).Scan(&afterJobs, &afterEvents); err != nil {
		t.Fatal(err)
	}
	afterInputs, _ := os.ReadDir(filepath.Join(root, "inputs"))
	if afterJobs != beforeJobs || afterEvents != beforeEvents || len(afterInputs) != len(beforeInputs) {
		t.Fatal("outbox rejection left rows or leaked upload")
	}

	// A real processor failure persists failed status and never opens a download.
	missing, err := f.jobs.CreateThumbnail(context.Background(), jobs.ThumbnailPayload{InputID: "00000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	failed := f.waitForStatus(t, missing.ID, jobs.StatusFailed)
	if failed.ErrorCode == nil || failed.AttemptCount != 1 {
		t.Fatalf("failed job=%+v", failed)
	}
	v4Download(t, server, missing.ID, 409)
	v4Download(t, server, "00000000-0000-4000-8000-000000000002", 404)
	upload([]byte("GIF89a"), 415)
	upload(nil, 400)
	f.waitForACKs(t, 4)
	stopWorker(t, worker)
	f.assertSettled(t)
	for _, id := range []string{jpegJob.ID, pngJob.ID, shaDone.ID, missing.ID} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			var published bool
			err := f.pool.QueryRow(context.Background(), `SELECT published_at IS NOT NULL FROM cloudforge.outbox_events WHERE aggregate_id=$1`, id).Scan(&published)
			if err != nil {
				t.Fatal(err)
			}
			if published {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("confirmed event not marked published")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	staging, _ := os.ReadDir(filepath.Join(root, "staging"))
	if len(staging) != 0 {
		t.Fatal("staging files leaked")
	}
	t.Log("SHA compatible; outbox rollback cleanup and persisted failure verified; four real ACKs and settled broker queue")
}

func v4Image(t *testing.T, w, h int, format string, alpha uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 40, G: 80, B: 120, A: alpha})
		}
	}
	var buf bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&buf, img, nil)
	} else {
		err = png.Encode(&buf, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func v4Download(t *testing.T, server *httptest.Server, id string, want int) []byte {
	t.Helper()
	res, err := server.Client().Get(server.URL + "/v1/jobs/" + id + "/thumbnail")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("download %s status=%d want=%d body=%s", id, res.StatusCode, want, data)
	}
	if want == 200 && (res.Header.Get("Content-Type") != "image/png" || res.Header.Get("Content-Disposition") != fmt.Sprintf(`attachment; filename="thumbnail-%s.png"`, id)) {
		t.Fatalf("download headers=%v", res.Header)
	}
	return data
}

func v4AssertThumbnail(t *testing.T, server *httptest.Server, job jobs.Job, w, h int, alpha uint8) {
	t.Helper()
	if job.AttemptCount != 1 || job.ClaimedBy == nil || *job.ClaimedBy != "v4-worker" {
		t.Fatalf("job=%+v", job)
	}
	data := v4Download(t, server, job.ID, 200)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		t.Fatalf("dimensions=%v want %dx%d", img.Bounds(), w, h)
	}
	_, _, _, a := img.At(w/2, h/2).RGBA()
	if a != uint32(alpha)*257 {
		t.Fatalf("alpha=%d want=%d", a, uint32(alpha)*257)
	}
}
