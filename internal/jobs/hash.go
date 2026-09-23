package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

func Execute(ctx context.Context, job Job) (json.RawMessage, error) {
	if job.Type != TypeSyntheticSHA256 {
		return nil, fmt.Errorf("unsupported job type %q", job.Type)
	}

	var payload SHA256Payload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode SHA-256 payload: %w", err)
	}
	if err := ValidateCreate(CreateInput{Type: job.Type, Payload: payload}); err != nil {
		return nil, fmt.Errorf("validate SHA-256 payload: %w", err)
	}

	started := time.Now()
	digest := sha256.Sum256([]byte(payload.Text))
	for i := 1; i < payload.Iterations; i++ {
		if i%10_000 == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
		digest = sha256.Sum256(digest[:])
	}

	result, err := json.Marshal(SHA256Result{
		Digest:     hex.EncodeToString(digest[:]),
		Iterations: payload.Iterations,
		DurationMS: time.Since(started).Milliseconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode SHA-256 result: %w", err)
	}
	return result, nil
}
