package jobs

import "testing"

func TestValidateCreate(t *testing.T) {
	tests := []struct {
		name  string
		input CreateInput
		valid bool
	}{
		{name: "valid", input: CreateInput{Type: TypeSyntheticSHA256, Payload: SHA256Payload{Text: "hello", Iterations: 1}}, valid: true},
		{name: "invalid type", input: CreateInput{Type: "image.resize", Payload: SHA256Payload{Text: "hello", Iterations: 1}}},
		{name: "empty text", input: CreateInput{Type: TypeSyntheticSHA256, Payload: SHA256Payload{Iterations: 1}}},
		{name: "below iteration minimum", input: CreateInput{Type: TypeSyntheticSHA256, Payload: SHA256Payload{Text: "hello", Iterations: 0}}},
		{name: "above iteration maximum", input: CreateInput{Type: TypeSyntheticSHA256, Payload: SHA256Payload{Text: "hello", Iterations: MaxIterations + 1}}},
		{name: "text too large", input: CreateInput{Type: TypeSyntheticSHA256, Payload: SHA256Payload{Text: string(make([]byte, MaxTextBytes+1)), Iterations: 1}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreate(tt.input)
			if tt.valid && err != nil {
				t.Fatalf("ValidateCreate() error = %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("ValidateCreate() expected an error")
			}
		})
	}
}
