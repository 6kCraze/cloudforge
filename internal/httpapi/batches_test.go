package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
)

type fakeBatchJobs struct {
	fakeImageJobs
	create func(context.Context, []jobs.BatchInput) (jobs.Batch, error)
	get    func(context.Context, string) (jobs.Batch, error)
}

func (f fakeBatchJobs) CreateBatch(ctx context.Context, inputs []jobs.BatchInput) (jobs.Batch, error) {
	return f.create(ctx, inputs)
}
func (f fakeBatchJobs) GetBatch(ctx context.Context, id string) (jobs.Batch, error) {
	return f.get(ctx, id)
}

func multipartBatch(t *testing.T, data ...[]byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, image := range data {
		part, err := writer.CreateFormFile("images", "photo.png")
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
	return body.Bytes(), writer.FormDataContentType()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestBatchRequestBoundariesAndCleanup(t *testing.T) {
	png := tinyPNG(t)
	for _, tc := range []struct {
		name      string
		data      [][]byte
		status    int
		epilogue  bool
		declared  bool
		truncated bool
	}{
		{name: "no files", status: 400},
		{name: "fifth file", data: [][]byte{png, png, png, png, png}, status: 400},
		{name: "bad second file", data: [][]byte{png, []byte("GIF89a")}, status: 415},
		{name: "per image limit", data: [][]byte{png, make([]byte, images.MaxInputBytes+1)}, status: 413},
		{name: "chunked total limit including epilogue", data: [][]byte{png}, status: 413, epilogue: true},
		{name: "declared total limit", status: 413, declared: true},
		{name: "truncated multipart", data: [][]byte{png}, status: 400, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			storage, err := images.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			service := fakeBatchJobs{create: func(context.Context, []jobs.BatchInput) (jobs.Batch, error) {
				t.Fatal("rejected batch registered")
				return jobs.Batch{}, nil
			}}
			api := NewWithImages(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), storage)
			body, ctype := multipartBatch(t, tc.data...)
			var reader io.Reader = bytes.NewReader(body)
			if tc.epilogue {
				reader = io.MultiReader(reader, io.LimitReader(zeroReader{}, MaxBatchBodyBytes))
			}
			if tc.truncated {
				reader = bytes.NewReader(body[:len(body)-20])
			}
			req := httptest.NewRequest("POST", "/v1/batches", reader)
			req.Header.Set("Content-Type", ctype)
			req.ContentLength = -1
			if tc.declared {
				req.ContentLength = MaxBatchBodyBytes + 1
			}
			res := httptest.NewRecorder()
			api.Handler().ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", res.Code, tc.status, res.Body.String())
			}
			for _, dir := range []string{"staging", "inputs", "outputs"} {
				files, err := os.ReadDir(filepath.Join(root, dir))
				if err != nil || len(files) != 0 {
					t.Fatalf("%s leaked: %v %v", dir, files, err)
				}
			}
		})
	}
}

func TestBatchCommitFailureProtectsInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		keep int
	}{{"rollback", errors.New("rollback"), 0}, {"uncertain", jobs.ErrCommitUncertain, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			storage, err := images.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			service := fakeBatchJobs{create: func(_ context.Context, inputs []jobs.BatchInput) (jobs.Batch, error) {
				if len(inputs) != 2 {
					t.Fatal("missing metadata")
				}
				for _, input := range inputs {
					if _, err := os.Stat(filepath.Join(root, "inputs", input.InputID, "image")); err != nil {
						t.Fatal(err)
					}
				}
				return jobs.Batch{}, tc.err
			}}
			body, ctype := multipartBatch(t, tinyPNG(t), tinyPNG(t))
			req := httptest.NewRequest("POST", "/v1/batches", bytes.NewReader(body))
			req.Header.Set("Content-Type", ctype)
			res := httptest.NewRecorder()
			NewWithImages(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), storage).Handler().ServeHTTP(res, req)
			inputs, _ := os.ReadDir(filepath.Join(root, "inputs"))
			if res.Code != 500 || len(inputs) != tc.keep {
				t.Fatalf("status=%d inputs=%d", res.Code, len(inputs))
			}
		})
	}
}

func TestBrowserAssetsAndBatchRetrievalErrors(t *testing.T) {
	storage, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	service := fakeBatchJobs{get: func(context.Context, string) (jobs.Batch, error) { return jobs.Batch{}, jobs.ErrNotFound }}
	api := NewWithImages(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)), storage)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/", 200}, {"/assets/app.js", 200}, {"/assets/app.css", 200}, {"/assets/icon.svg", 200}, {"/assets/", 404}, {"/v1/batches/not-a-uuid", 400}, {"/v1/batches/00000000-0000-4000-8000-000000000001", 404}} {
		res := httptest.NewRecorder()
		api.Handler().ServeHTTP(res, httptest.NewRequest("GET", tc.path, nil))
		if res.Code != tc.status {
			t.Fatalf("%s status=%d", tc.path, res.Code)
		}
	}
	if name := displayName("\x00" + string(bytes.Repeat([]byte("界"), 210))); len([]rune(name)) != 200 {
		t.Fatal("display name not bounded")
	}
}
