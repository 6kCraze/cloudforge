package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/google/uuid"
)

type fakeImageJobs struct {
	fakeJobs
	thumbnail func(context.Context, jobs.ThumbnailPayload) (jobs.Job, error)
}

func (f fakeImageJobs) CreateThumbnail(ctx context.Context, p jobs.ThumbnailPayload) (jobs.Job, error) {
	return f.thumbnail(ctx, p)
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 80})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUploadDatabaseFailureCleanupAndUncertainCommitRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   error
		remaining int
	}{
		{"rolled back", errors.New("outbox insert rejected"), 0},
		{"commit uncertain", jobs.ErrCommitUncertain, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			storage, err := images.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			service := fakeImageJobs{thumbnail: func(_ context.Context, p jobs.ThumbnailPayload) (jobs.Job, error) {
				if _, err := os.Stat(filepath.Join(root, "inputs", p.InputID, "image")); err != nil {
					t.Fatalf("input not finalized before DB creation: %v", err)
				}
				return jobs.Job{}, tc.failure
			}}
			api := NewWithImages(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), storage)
			res := httptest.NewRecorder()
			api.Handler().ServeHTTP(res, httptest.NewRequest("POST", "/v1/images", bytes.NewReader(tinyPNG(t))))
			if res.Code != 500 {
				t.Fatalf("status=%d", res.Code)
			}
			inputs, err := os.ReadDir(filepath.Join(root, "inputs"))
			if err != nil || len(inputs) != tc.remaining {
				t.Fatalf("inputs=%v err=%v", inputs, err)
			}
			staging, _ := os.ReadDir(filepath.Join(root, "staging"))
			if len(staging) != 0 {
				t.Fatal("staging leaked")
			}
		})
	}
}

func TestUploadErrorsNeverCreateJobs(t *testing.T) {
	root := t.TempDir()
	storage, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	service := fakeImageJobs{thumbnail: func(context.Context, jobs.ThumbnailPayload) (jobs.Job, error) {
		t.Fatal("invalid upload created a job")
		return jobs.Job{}, nil
	}}
	api := NewWithImages(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), storage)
	for _, tc := range []struct {
		name   string
		data   []byte
		length int64
		want   int
	}{
		{"unsupported", []byte("GIF89a"), 6, 415},
		{"empty", nil, 0, 400},
		{"truncated", tinyPNG(t)[:30], 30, 400},
		{"declared oversized", nil, images.MaxInputBytes + 1, 413},
		{"unknown length oversized", make([]byte, images.MaxInputBytes+1), -1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/images", bytes.NewReader(tc.data))
			req.ContentLength = tc.length
			res := httptest.NewRecorder()
			api.Handler().ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestDownloadGuardsAndIncompleteOutput(t *testing.T) {
	root := t.TempDir()
	storage, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	input, err := storage.SaveInput(bytes.NewReader(tinyPNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(jobs.ThumbnailPayload{InputID: input.ID})
	job := jobs.Job{ID: uuid.NewString(), Type: jobs.TypeImageThumbnail, Payload: payload, Status: jobs.StatusSucceeded}
	job.Result, err = storage.Execute(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	service := fakeImageJobs{fakeJobs: fakeJobs{get: func(context.Context, string) (jobs.Job, error) { return job, nil }}}
	api := NewWithImages(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), storage)
	request := func(want int) {
		t.Helper()
		res := httptest.NewRecorder()
		api.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/jobs/"+job.ID+"/thumbnail", nil))
		if res.Code != want {
			t.Fatalf("status=%d want %d body=%s", res.Code, want, res.Body.String())
		}
	}
	for _, status := range []string{jobs.StatusQueued, jobs.StatusRunning, jobs.StatusFailed} {
		job.Status = status
		request(409)
	}
	job.Status = jobs.StatusSucceeded
	result := job.Result
	job.Result = json.RawMessage(`{"output_id":"` + uuid.NewString() + `","width":2,"height":2,"format":"png","bytes":90}`)
	request(409)
	job.Result = nil
	request(409)
	job.Result = result
	request(200)
	outputPath := filepath.Join(root, "outputs", job.ID, "thumbnail.png")
	corrupted, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted[len(corrupted)-1] ^= 1 // Same byte count and dimensions, invalid IEND checksum.
	if err := os.WriteFile(outputPath, corrupted, 0600); err != nil {
		t.Fatal(err)
	}
	request(409)
	if err := os.WriteFile(filepath.Join(root, "outputs", job.ID, "thumbnail.png"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	request(409)
	if err := os.Remove(filepath.Join(root, "outputs", job.ID, "thumbnail.png")); err != nil {
		t.Fatal(err)
	}
	request(404)
}
