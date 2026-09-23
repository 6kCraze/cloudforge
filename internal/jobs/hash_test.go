package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestExecuteSHA256IsDeterministic(t *testing.T) {
	payload, err := json.Marshal(SHA256Payload{Text: "hello world", Iterations: 2})
	if err != nil {
		t.Fatal(err)
	}
	job := Job{Type: TypeSyntheticSHA256, Payload: payload}

	raw, err := Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var got SHA256Result
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	first := sha256.Sum256([]byte("hello world"))
	want := sha256.Sum256(first[:])
	if got.Digest != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %s, want %s", got.Digest, hex.EncodeToString(want[:]))
	}
	if got.Iterations != 2 {
		t.Fatalf("iterations = %d, want 2", got.Iterations)
	}
}
