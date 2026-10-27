package images

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"

	"github.com/6kCraze/cloudforge/internal/jobs"
	"golang.org/x/image/draw"
)

func (s *Store) Execute(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
	if job.Type == jobs.TypeSyntheticSHA256 {
		return jobs.Execute(ctx, job)
	}
	if job.Type != jobs.TypeImageThumbnail {
		return nil, fmt.Errorf("unsupported job type %q", job.Type)
	}
	var payload jobs.ThumbnailPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode thumbnail payload: %w", err)
	}
	if !validID(payload.InputID) || !validID(job.ID) {
		return nil, fmt.Errorf("invalid thumbnail identifiers")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := s.root.Open("inputs/" + payload.InputID + "/image")
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	img, _, decodeErr := decodeBounded(input)
	_ = input.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	w, h := fit(img.Bounds().Dx(), img.Bounds().Dy())
	thumbnail := image.NewNRGBA(image.Rect(0, 0, w, h))
	// ApproxBiLinear bounds scaling work while retaining alpha with Src.
	draw.ApproxBiLinear.Scale(thumbnail, thumbnail.Bounds(), img, img.Bounds(), draw.Src, nil)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, stage, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer func() { _ = output.Close(); _ = s.root.Remove(stage) }()
	if err := png.Encode(&limitedWriter{writer: output, remaining: MaxOutputBytes}, thumbnail); err != nil {
		return nil, err
	}
	if err := output.Sync(); err != nil {
		return nil, err
	}
	stat, err := output.Stat()
	if err != nil {
		return nil, err
	}
	if err := output.Close(); err != nil {
		return nil, err
	}
	if err := s.finalize(stage, "outputs", job.ID, "thumbnail.png"); err != nil {
		return nil, fmt.Errorf("finalize thumbnail: %w", err)
	}
	return json.Marshal(jobs.ThumbnailResult{OutputID: job.ID, Width: w, Height: h, Format: "png", Bytes: stat.Size()})
}

func fit(w, h int) (int, int) {
	if w <= ThumbnailSize && h <= ThumbnailSize {
		return w, h
	}
	if w >= h {
		return ThumbnailSize, max(1, h*ThumbnailSize/w)
	}
	return max(1, w*ThumbnailSize/h), ThumbnailSize
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("thumbnail exceeds output limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
