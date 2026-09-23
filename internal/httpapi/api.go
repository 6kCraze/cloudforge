package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/google/uuid"
)

type JobService interface {
	Create(context.Context, jobs.CreateInput) (jobs.Job, error)
	Get(context.Context, string) (jobs.Job, error)
}

type ReadinessChecker interface {
	Ping(context.Context) error
}

type API struct {
	jobs   JobService
	db     ReadinessChecker
	logger *slog.Logger
}

func New(jobService JobService, db ReadinessChecker, logger *slog.Logger) *API {
	return &API{jobs: jobService, db: db, logger: logger}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("POST /v1/jobs", a.createJob)
	mux.HandleFunc("GET /v1/jobs/{id}", a.getJob)
	return mux
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(r.Context()); err != nil {
		a.logger.Error("readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database is unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request) {
	var input jobs.CreateInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object", "")
		return
	}

	job, err := a.jobs.Create(r.Context(), input)
	if err != nil {
		var validationErr *jobs.ValidationError
		if errors.As(err, &validationErr) {
			writeError(w, http.StatusBadRequest, "invalid_request", validationErr.Message, validationErr.Field)
			return
		}
		a.logger.Error("create job failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "unable to create job", "")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID, "status": job.Status})
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_job_id", "job ID must be a UUID", "id")
		return
	}

	job, err := a.jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "job was not found", "")
		return
	}
	if err != nil {
		a.logger.Error("get job failed", "job_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "unable to retrieve job", "")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

type errorEnvelope struct {
	Error structuredError `json:"error"`
}

type structuredError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message, field string) {
	writeJSON(w, status, errorEnvelope{Error: structuredError{Code: code, Message: message, Field: field}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
