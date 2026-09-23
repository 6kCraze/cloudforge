package jobs

import "unicode/utf8"

func ValidateCreate(input CreateInput) error {
	if input.Type != TypeSyntheticSHA256 {
		return &ValidationError{Field: "type", Message: "must be synthetic.sha256"}
	}
	if input.Payload.Text == "" {
		return &ValidationError{Field: "payload.text", Message: "must not be empty"}
	}
	if !utf8.ValidString(input.Payload.Text) {
		return &ValidationError{Field: "payload.text", Message: "must be valid UTF-8"}
	}
	if len([]byte(input.Payload.Text)) > MaxTextBytes {
		return &ValidationError{Field: "payload.text", Message: "must be at most 65536 bytes"}
	}
	if input.Payload.Iterations < MinIterations || input.Payload.Iterations > MaxIterations {
		return &ValidationError{Field: "payload.iterations", Message: "must be between 1 and 50000000"}
	}
	return nil
}
