package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/google/uuid"
)

type ThumbnailCreator interface {
	CreateThumbnail(context.Context, jobs.ThumbnailPayload) (jobs.Job, error)
}

type ImageJobService interface {
	JobService
	ThumbnailCreator
}

func NewWithImages(service ImageJobService, db ReadinessChecker, logger *slog.Logger, storage *images.Store) *API {
	api := New(service, db, logger)
	api.images = storage
	api.imageJobs = service
	if batches, ok := service.(jobs.BatchStore); ok {
		api.batches = batches
	}
	return api
}

// A raw body holds exactly one image; its MIME type and filename are not trusted.
func (a *API) uploadImage(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > images.MaxInputBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "image_too_large", images.ErrTooLarge.Error(), "image")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, images.MaxInputBytes)
	input, err := a.images.SaveInput(r.Body)
	if err != nil {
		a.writeUploadError(w, err)
		return
	}
	job, err := a.imageJobs.CreateThumbnail(r.Context(), jobs.ThumbnailPayload{InputID: input.ID})
	if err != nil {
		if !errors.Is(err, jobs.ErrCommitUncertain) {
			if cleanupErr := a.images.RemoveInput(input.ID); cleanupErr != nil {
				a.logger.Error("input cleanup failed", "input_id", input.ID, "error", cleanupErr)
			}
		}
		a.logger.Error("create image job failed", "input_id", input.ID, "job_id", job.ID, "input_retained", errors.Is(err, jobs.ErrCommitUncertain), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "unable to confirm image job creation", "")
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID, "status": job.Status})
}

func (a *API) writeUploadError(w http.ResponseWriter, err error) {
	var maxBody *http.MaxBytesError
	switch {
	case errors.Is(err, images.ErrTooLarge) || errors.As(err, &maxBody):
		writeError(w, http.StatusRequestEntityTooLarge, "image_too_large", images.ErrTooLarge.Error(), "image")
	case errors.Is(err, images.ErrDimensions):
		writeError(w, http.StatusUnprocessableEntity, "image_dimensions", err.Error(), "image")
	case errors.Is(err, images.ErrUnsupported):
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_image", err.Error(), "image")
	case errors.Is(err, images.ErrInvalid) || errors.Is(err, io.ErrUnexpectedEOF):
		writeError(w, http.StatusBadRequest, "invalid_image", err.Error(), "image")
	case errors.Is(err, images.ErrStorageFull):
		writeError(w, http.StatusInsufficientStorage, "storage_full", err.Error(), "")
	default:
		a.logger.Error("store upload failed", "error", err)
		writeError(w, http.StatusInternalServerError, "storage_error", "unable to store image", "")
	}

}

func (a *API) downloadThumbnail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, 400, "invalid_job_id", "job ID must be a UUID", "id")
		return
	}
	job, err := a.jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, 404, "job_not_found", "job was not found", "")
		return
	}
	if err != nil {
		a.logger.Error("get thumbnail job failed", "job_id", id, "error", err)
		writeError(w, 500, "internal_error", "unable to retrieve job", "")
		return
	}
	if job.Type != jobs.TypeImageThumbnail {
		writeError(w, 404, "thumbnail_not_available", "job has no thumbnail", "")
		return
	}
	if job.Status != jobs.StatusSucceeded {
		writeError(w, 409, "job_not_succeeded", "thumbnail requires a succeeded job", "")
		return
	}
	var result jobs.ThumbnailResult
	if err := json.Unmarshal(job.Result, &result); err != nil || result.OutputID != job.ID || result.Format != "png" || result.Width < 1 || result.Height < 1 || result.Width > images.ThumbnailSize || result.Height > images.ThumbnailSize || result.Bytes <= 0 || result.Bytes > images.MaxOutputBytes {
		writeError(w, 409, "incomplete_result", "job has no complete thumbnail result", "")
		return
	}
	if result.AttemptID != "" {
		attempt, parseErr := uuid.Parse(result.AttemptID)
		if parseErr != nil || attempt.String() != result.AttemptID {
			writeError(w, 409, "incomplete_result", "job has invalid thumbnail attempt", "")
			return
		}
	}
	file, err := a.images.OpenOutput(job.ID, result.AttemptID)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, 404, "output_missing", "thumbnail file was not found", "")
		return
	}
	if err != nil {
		a.logger.Error("open thumbnail failed", "job_id", job.ID, "error", err)
		writeError(w, 500, "storage_error", "unable to open thumbnail", "")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != result.Bytes {
		writeError(w, 409, "incomplete_output", "thumbnail file is incomplete", "")
		return
	}
	cfg, err := png.DecodeConfig(io.LimitReader(file, images.MaxOutputBytes+1))
	if err != nil || cfg.Width != result.Width || cfg.Height != result.Height {
		writeError(w, 409, "incomplete_output", "thumbnail metadata does not match result", "")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, 500, "storage_error", "unable to read thumbnail", "")
		return
	}
	// Output is at most 320x320: validate the full PNG, including its data and
	// checksums, before serving it. Header validity alone cannot prove completion.
	if _, err := png.Decode(io.LimitReader(file, images.MaxOutputBytes+1)); err != nil {
		writeError(w, 409, "incomplete_output", "thumbnail file is malformed or incomplete", "")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, 500, "storage_error", "unable to read thumbnail", "")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `attachment; filename="thumbnail-`+job.ID+`.png"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "thumbnail.png", stat.ModTime(), file)
}
