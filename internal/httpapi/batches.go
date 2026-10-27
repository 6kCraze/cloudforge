package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/google/uuid"
)

const MaxBatchBodyBytes int64 = 21 << 20

func (a *API) createBatch(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > MaxBatchBodyBytes {
		writeError(w, 413, "batch_too_large", "batch request must be at most 21 MiB", "images")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBatchBodyBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "invalid_multipart", "send images as multipart/form-data", "images")
		return
	}
	inputs := make([]jobs.BatchInput, 0, jobs.MaxBatchImages)
	retain := false
	defer func() {
		if !retain {
			for _, input := range inputs {
				if err := a.images.RemoveInput(input.InputID); err != nil {
					a.logger.Error("batch input cleanup failed", "input_id", input.InputID, "error", err)
				}
			}
		}
	}()
	multipartError := func(err error) {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			writeError(w, 413, "batch_too_large", "batch request must be at most 21 MiB", "images")
		} else {
			writeError(w, 400, "invalid_multipart", "batch upload is incomplete or malformed", "images")
		}
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			multipartError(err)
			return
		}
		if len(inputs) >= jobs.MaxBatchImages {
			writeError(w, 400, "too_many_images", "select at most four images", "images")
			return
		}
		if part.FormName() != "images" || part.FileName() == "" {
			writeError(w, 400, "invalid_part", "each part must be an image file named images", "images")
			return
		}
		// SaveInput streams, validates and finalizes one image at a time. Only
		// small metadata is retained in memory while the next part is decoded.
		input, err := a.images.SaveInput(part)
		if err != nil {
			a.writeUploadError(w, err)
			return
		}
		inputs = append(inputs, jobs.BatchInput{InputID: input.ID, OriginalName: displayName(part.FileName()), Format: input.Format, Bytes: input.Bytes, Width: input.Width, Height: input.Height})
		if err := part.Close(); err != nil {
			multipartError(err)
			return
		}
	}
	// MultipartReader can stop at the closing boundary before a trailing
	// epilogue. Consume the remaining body so chunked requests obey 21 MiB too.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		multipartError(err)
		return
	}
	if len(inputs) == 0 {
		writeError(w, 400, "no_images", "select between one and four images", "images")
		return
	}
	batch, err := a.batches.CreateBatch(r.Context(), inputs)
	if err != nil {
		retain = errors.Is(err, jobs.ErrCommitUncertain)
		ids := make([]string, 0, len(inputs))
		for _, input := range inputs {
			ids = append(ids, input.InputID)
		}
		a.logger.Error("create batch failed", "batch_id", batch.ID, "input_ids", ids, "inputs_retained", retain, "error", err)
		writeError(w, 500, "batch_creation_failed", "unable to confirm batch creation; check the batch before resubmitting", "")
		return
	}
	retain = true
	w.Header().Set("Location", "/v1/batches/"+batch.ID)
	writeJSON(w, 202, batch)
}

// Display names are bounded metadata, never filesystem paths or HTML markup.
func displayName(name string) string {
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "Image"
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}
	return name
}

func (a *API) getBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, 400, "invalid_batch_id", "batch ID must be a UUID", "id")
		return
	}
	batch, err := a.batches.GetBatch(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, 404, "batch_not_found", "batch was not found", "")
		return
	}
	if err != nil {
		a.logger.Error("get batch failed", "batch_id", id, "error", err)
		writeError(w, 500, "internal_error", "unable to retrieve batch", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, batch)
}
