package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6kCraze/cloudforge/internal/jobs"
)

type fakeJobs struct {
	create func(context.Context, jobs.CreateInput) (jobs.Job, error)
	get    func(context.Context, string) (jobs.Job, error)
}

func (f fakeJobs) Create(ctx context.Context, input jobs.CreateInput) (jobs.Job, error) {
	return f.create(ctx, input)
}

func (f fakeJobs) Get(ctx context.Context, id string) (jobs.Job, error) {
	return f.get(ctx, id)
}

type readyDB struct{}

func (readyDB) Ping(context.Context) error { return nil }

func TestCreateJobReturnsAccepted(t *testing.T) {
	service := fakeJobs{
		create: func(_ context.Context, input jobs.CreateInput) (jobs.Job, error) {
			if input.Payload.Text != "hello" {
				t.Fatalf("text = %q, want hello", input.Payload.Text)
			}
			return jobs.Job{ID: "018f0f8c-7747-7f64-8bc7-8ce8a061fe9c", Status: jobs.StatusQueued}, nil
		},
		get: func(context.Context, string) (jobs.Job, error) { return jobs.Job{}, jobs.ErrNotFound },
	}
	api := New(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"type":"synthetic.sha256","payload":{"text":"hello","iterations":10}}`))
	res := httptest.NewRecorder()

	api.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", res.Code, res.Body.String())
	}
}

func TestCreateJobReturnsStructuredValidationError(t *testing.T) {
	service := fakeJobs{
		create: func(context.Context, jobs.CreateInput) (jobs.Job, error) {
			return jobs.Job{}, &jobs.ValidationError{Field: "payload.iterations", Message: "out of bounds"}
		},
		get: func(context.Context, string) (jobs.Job, error) { return jobs.Job{}, jobs.ErrNotFound },
	}
	api := New(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"type":"synthetic.sha256","payload":{"text":"hello","iterations":0}}`))
	res := httptest.NewRecorder()

	api.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	var body errorEnvelope
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "invalid_request" || body.Error.Field != "payload.iterations" {
		t.Fatalf("error = %#v", body.Error)
	}
}

func TestGetUnknownJobReturnsNotFound(t *testing.T) {
	service := fakeJobs{
		create: func(context.Context, jobs.CreateInput) (jobs.Job, error) { return jobs.Job{}, errors.New("unused") },
		get:    func(context.Context, string) (jobs.Job, error) { return jobs.Job{}, jobs.ErrNotFound },
	}
	api := New(service, readyDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/018f0f8c-7747-7f64-8bc7-8ce8a061fe9c", nil)
	res := httptest.NewRecorder()

	api.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
}
