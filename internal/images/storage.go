package images

import (
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const (
	MaxInputBytes  int64 = 5 << 20
	MaxPixels      int64 = 8_000_000
	MaxDimension         = 8192
	ThumbnailSize        = 320
	MaxOutputBytes int64 = 512 << 10
	// Conservative admission budget includes output space reserved per input.
	MaxStorageBytes int64 = 256 << 20
)

var (
	ErrTooLarge    = errors.New("image must be at most 5 MiB")
	ErrUnsupported = errors.New("image contents must be JPEG or PNG")
	ErrInvalid     = errors.New("image is malformed or incomplete")
	ErrDimensions  = errors.New("image must be at most 8 megapixels and 8192 pixels per dimension")
	ErrStorageFull = errors.New("local storage admission budget is full")
)

type Input struct {
	ID            string
	Width, Height int
	Format        string
	Bytes         int64
}

type Store struct {
	root *os.Root
	// One API upload is decoded at a time, avoiding concurrent 8 MP allocations.
	uploadMu sync.Mutex
}

func Open(rootPath string) (*Store, error) {
	if !filepath.IsAbs(rootPath) {
		return nil, errors.New("STORAGE_ROOT must be an absolute path")
	}
	if err := os.MkdirAll(rootPath, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"staging", "inputs", "outputs"} {
		if err := root.MkdirAll(name, 0700); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

func (s *Store) Close() error { return s.root.Close() }

func validID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func (s *Store) stage() (*os.File, string, error) {
	name := "staging/" + uuid.NewString() + ".tmp"
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	return file, name, err
}

// The exclusive directory reserves the destination before rename, so an
// existing result/input cannot be replaced. The closed file is on the same root.
func (s *Store) finalize(stage, category, id, filename string) error {
	if !validID(id) {
		return errors.New("invalid storage identifier")
	}
	dir := category + "/" + id
	if err := s.root.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err := s.root.Rename(stage, dir+"/"+filename); err != nil {
		_ = s.root.Remove(dir) // Remove only the empty directory we reserved.
		return err
	}
	return nil
}

// An attempt reserves its own directory. Even a stale worker may finish a
// candidate file, but it cannot replace another attempt's bytes.
func (s *Store) finalizeAttempt(stage, jobID, attemptID string) error {
	if !validID(jobID) || !validID(attemptID) {
		return errors.New("invalid output identifier")
	}
	jobDir := "outputs/" + jobID
	if err := s.root.Mkdir(jobDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	attemptDir := jobDir + "/" + attemptID
	if err := s.root.Mkdir(attemptDir, 0700); err != nil {
		return err
	}
	if err := s.root.Rename(stage, attemptDir+"/thumbnail.png"); err != nil {
		_ = s.root.Remove(attemptDir)
		return err
	}
	return nil
}

func (s *Store) checkBudget() error {
	var used, inputs int64
	err := fs.WalkDir(s.root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} // Worker may finalize a staging file during the scan.
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("storage may not contain symlinks")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		used += info.Size()
		if strings.HasPrefix(name, "inputs/") {
			inputs++
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Reserve a maximum thumbnail for every input, including queued inputs.
	if used+MaxInputBytes+(inputs+1)*MaxOutputBytes > MaxStorageBytes {
		return ErrStorageFull
	}
	return nil
}

func (s *Store) SaveInput(reader io.Reader) (Input, error) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if err := s.checkBudget(); err != nil {
		return Input{}, err
	}
	file, stage, err := s.stage()
	if err != nil {
		return Input{}, err
	}
	defer func() { _ = file.Close(); _ = s.root.Remove(stage) }()
	size, err := io.Copy(file, io.LimitReader(reader, MaxInputBytes+1))
	if size > MaxInputBytes {
		return Input{}, ErrTooLarge
	}
	if err != nil {
		return Input{}, err
	}
	if size == 0 {
		return Input{}, ErrInvalid
	}
	if err := file.Sync(); err != nil {
		return Input{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Input{}, err
	}
	img, format, err := decodeBounded(file)
	if err != nil {
		return Input{}, err
	}
	info := Input{ID: uuid.NewString(), Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), Format: format, Bytes: size}
	if err := file.Close(); err != nil {
		return Input{}, err
	}
	if err := s.finalize(stage, "inputs", info.ID, "image"); err != nil {
		return Input{}, err
	}
	return info, nil
}

func decodeBounded(file *os.File) (image.Image, string, error) {
	stat, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !stat.Mode().IsRegular() {
		return nil, "", ErrInvalid
	}
	if stat.Size() > MaxInputBytes {
		return nil, "", ErrTooLarge
	}
	cfg, format, err := image.DecodeConfig(io.LimitReader(file, MaxInputBytes+1))
	if errors.Is(err, image.ErrFormat) {
		return nil, "", ErrUnsupported
	}
	if err != nil {
		return nil, "", ErrInvalid
	}
	if format != "jpeg" && format != "png" {
		return nil, "", ErrUnsupported
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxDimension || cfg.Height > MaxDimension || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, "", ErrDimensions
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	img, decodedFormat, err := image.Decode(io.LimitReader(file, MaxInputBytes+1))
	if err != nil || decodedFormat != format {
		return nil, "", ErrInvalid
	}
	if img.Bounds().Dx() != cfg.Width || img.Bounds().Dy() != cfg.Height {
		return nil, "", ErrInvalid
	}
	return img, format, nil
}

func (s *Store) RemoveInput(id string) error {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if !validID(id) {
		return errors.New("invalid input identifier")
	}
	return s.root.RemoveAll("inputs/" + id)
}

func (s *Store) OpenOutput(jobID string, attemptID ...string) (*os.File, error) {
	if !validID(jobID) {
		return nil, fmt.Errorf("invalid output identifier")
	}
	path := "outputs/" + jobID + "/thumbnail.png" // V4 result without attempt_id.
	if len(attemptID) > 0 && attemptID[0] != "" {
		if !validID(attemptID[0]) {
			return nil, fmt.Errorf("invalid attempt identifier")
		}
		path = "outputs/" + jobID + "/" + attemptID[0] + "/thumbnail.png"
	}
	return s.root.Open(path)
}
