package outbox

const EventTypeJobReady = "job.ready"

// JobReadyPayload is the future RabbitMQ message body. The event ID and type
// remain first-class outbox fields and can become message metadata later.
type JobReadyPayload struct {
	JobID string `json:"job_id"`
}
