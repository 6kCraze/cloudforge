package outbox

import (
	"encoding/json"
	"time"
)

const EventTypeJobReady = "job.ready"

type Event struct {
	ID          string
	EventType   string
	AggregateID string
	Payload     json.RawMessage
	CreatedAt   time.Time
	PublishedAt *time.Time
}

// JobReadyPayload is the future RabbitMQ message body. The event ID and type
// remain first-class outbox fields and can become message metadata later.
type JobReadyPayload struct {
	JobID string `json:"job_id"`
}
