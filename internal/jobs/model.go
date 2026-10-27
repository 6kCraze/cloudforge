package jobs

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	TypeSyntheticSHA256 = "synthetic.sha256"
	TypeImageThumbnail  = "image.thumbnail"

	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusRetryWait = "retry_wait"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"

	MinIterations = 1
	MaxIterations = 50_000_000
	MaxTextBytes  = 64 * 1024
)

var (
	ErrNotFound        = errors.New("job not found")
	ErrNoJobs          = errors.New("no queued jobs")
	ErrNotClaimable    = errors.New("job is not queued")
	ErrCommitUncertain = errors.New("job commit outcome is uncertain")
	ErrOwnershipLost   = errors.New("job attempt no longer owns a valid lease")
	ErrPermanentJob    = errors.New("permanently invalid job")
)

type ThumbnailPayload struct {
	InputID string `json:"input_id"`
}

type ThumbnailResult struct {
	OutputID  string `json:"output_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Format    string `json:"format"`
	Bytes     int64  `json:"bytes"`
}

type SHA256Payload struct {
	Text       string `json:"text"`
	Iterations int    `json:"iterations"`
}

type SHA256Result struct {
	Digest     string `json:"digest"`
	Iterations int    `json:"iterations"`
	DurationMS int64  `json:"duration_ms"`
}

type CreateInput struct {
	Type    string        `json:"type"`
	Payload SHA256Payload `json:"payload"`
}

type Job struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	Result         json.RawMessage `json:"result"`
	ErrorCode      *string         `json:"error_code"`
	ErrorMessage   *string         `json:"error_message"`
	AttemptCount   int16           `json:"attempt_count"`
	ClaimedBy      *string         `json:"claimed_by"`
	AttemptID      *string         `json:"attempt_id,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	NextRetryAt    *time.Time      `json:"next_retry_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	StartedAt      *time.Time      `json:"started_at"`
	CompletedAt    *time.Time      `json:"completed_at"`
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}
