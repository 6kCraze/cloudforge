package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ListUnpublished performs one short SELECT. It intentionally holds no row lock
// or database transaction while the dispatcher communicates with RabbitMQ.
func (s *Store) ListUnpublished(ctx context.Context, limit int) ([]Event, error) {
	if limit < 1 {
		return nil, fmt.Errorf("list unpublished outbox events: limit must be positive")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, event_type, aggregate_id::text, payload, created_at, published_at
		FROM cloudforge.outbox_events
		WHERE published_at IS NULL
		ORDER BY created_at, id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("select unpublished outbox events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		if err := rows.Scan(
			&event.ID,
			&event.EventType,
			&event.AggregateID,
			&event.Payload,
			&event.CreatedAt,
			&event.PublishedAt,
		); err != nil {
			return nil, fmt.Errorf("scan unpublished outbox event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unpublished outbox events: %w", err)
	}
	return events, nil
}

func (s *Store) MarkPublished(ctx context.Context, eventID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloudforge.outbox_events
		SET published_at = now()
		WHERE id = $1
		  AND published_at IS NULL
	`, eventID)
	if err != nil {
		return fmt.Errorf("mark outbox event published: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("mark outbox event published: event %s is missing or already published", eventID)
	}
	return nil
}
