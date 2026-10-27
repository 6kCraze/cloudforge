package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/google/uuid"
)

func encodedImage(t *testing.T, w, h int, format string, alpha uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 40, G: 80, B: 120, A: alpha})
		}
	}
	var b bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&b, img, nil)
	} else {
		err = png.Encode(&b, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func openTestStorage(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func TestThumbnailDimensionsAlphaAndExclusivePublication(t *testing.T) {
	for _, tc := range []struct {
		name, format       string
		w, h, wantW, wantH int
		alpha              uint8
	}{
		{"jpeg landscape", "jpeg", 640, 320, 320, 160, 255},
		{"png portrait alpha", "png", 240, 480, 160, 320, 96},
		{"png small no upscale", "png", 48, 32, 48, 32, 0},
		{"very narrow stays nonzero", "png", 1, 640, 1, 320, 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, root := openTestStorage(t)
			input, err := s.SaveInput(bytes.NewReader(encodedImage(t, tc.w, tc.h, tc.format, tc.alpha)))
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(jobs.ThumbnailPayload{InputID: input.ID})
			job := jobs.Job{ID: uuid.NewString(), Type: jobs.TypeImageThumbnail, Payload: payload}
			raw, err := s.Execute(context.Background(), job)
			if err != nil {
				t.Fatal(err)
			}
			var result jobs.ThumbnailResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if result.OutputID != job.ID || result.Width != tc.wantW || result.Height != tc.wantH || result.Bytes <= 0 {
				t.Fatalf("result=%+v", result)
			}
			file, err := s.OpenOutput(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			output, err := png.Decode(file)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			if output.Bounds().Dx() != tc.wantW || output.Bounds().Dy() != tc.wantH {
				t.Fatalf("bounds=%v", output.Bounds())
			}
			if tc.format == "png" {
				_, _, _, alpha := output.At(tc.wantW/2, tc.wantH/2).RGBA()
				if alpha != uint32(tc.alpha)*257 {
					t.Fatalf("alpha=%d want %d", alpha, uint32(tc.alpha)*257)
				}
			}
			before, err := os.ReadFile(filepath.Join(root, "outputs", job.ID, "thumbnail.png"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Execute(context.Background(), job); err == nil {
				t.Fatal("existing output was overwritten")
			}
			after, _ := os.ReadFile(filepath.Join(root, "outputs", job.ID, "thumbnail.png"))
			if !bytes.Equal(before, after) {
				t.Fatal("existing output changed")
			}
			staging, _ := os.ReadDir(filepath.Join(root, "staging"))
			if len(staging) != 0 {
				t.Fatal("staging leaked")
			}
		})
	}
}

// Change only the PNG header and CRC: no huge pixel allocation is needed to
// verify that size validation happens before full decoding.
func pngHeaderDimensions(data []byte, w, h uint32) []byte {
	data = bytes.Clone(data)
	binary.BigEndian.PutUint32(data[16:20], w)
	binary.BigEndian.PutUint32(data[20:24], h)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func TestRejectedInputsLeaveNoFiles(t *testing.T) {
	valid := encodedImage(t, 2, 2, "png", 255)
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"unsupported", []byte("GIF89a"), ErrUnsupported},
		{"empty", nil, ErrInvalid},
		{"truncated", valid[:len(valid)-10], ErrInvalid},
		{"dimension header", pngHeaderDimensions(valid, 8193, 1), ErrDimensions},
		{"pixel header", pngHeaderDimensions(valid, 4000, 2001), ErrDimensions},
		{"byte limit", make([]byte, MaxInputBytes+1), ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, root := openTestStorage(t)
			if _, err := s.SaveInput(bytes.NewReader(tc.data)); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			for _, dir := range []string{"staging", "inputs", "outputs"} {
				entries, err := os.ReadDir(filepath.Join(root, dir))
				if err != nil || len(entries) != 0 {
					t.Fatalf("%s entries=%v err=%v", dir, entries, err)
				}
			}
		})
	}
}

func TestStorageAndWorkerRejectUnsafePathsAndTamperedInput(t *testing.T) {
	if _, err := Open("relative/storage"); err == nil {
		t.Fatal("relative root accepted")
	}
	s, root := openTestStorage(t)
	for _, id := range []string{"../escape", "C:/outside", "not-a-uuid"} {
		if _, err := s.OpenOutput(id); err == nil {
			t.Fatalf("unsafe output ID %q accepted", id)
		}
		if err := s.RemoveInput(id); err == nil {
			t.Fatalf("unsafe input ID %q accepted", id)
		}
	}
	input, err := s.SaveInput(bytes.NewReader(encodedImage(t, 2, 2, "png", 255)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inputs", input.ID, "image"), pngHeaderDimensions(encodedImage(t, 2, 2, "png", 255), 8193, 1), 0600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(jobs.ThumbnailPayload{InputID: input.ID})
	if _, err := s.Execute(context.Background(), jobs.Job{ID: uuid.NewString(), Type: jobs.TypeImageThumbnail, Payload: payload}); !errors.Is(err, ErrDimensions) {
		t.Fatalf("worker did not revalidate input: %v", err)
	}
}
