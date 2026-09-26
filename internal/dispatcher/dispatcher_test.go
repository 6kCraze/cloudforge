package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
)

type fakeStore struct {
	mu        sync.Mutex
	events    []outbox.Event
	marked    []string
	markErr   error
	listErr   error
	listCalls int
}

func (s *fakeStore) ListUnpublished(_ context.Context, limit int) ([]outbox.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	if limit > len(s.events) {
		limit = len(s.events)
	}
	return append([]outbox.Event(nil), s.events[:limit]...), nil
}

func (s *fakeStore) MarkPublished(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		return s.markErr
	}
	s.marked = append(s.marked, id)
	for i := range s.events {
		if s.events[i].ID == id {
			s.events = append(s.events[:i], s.events[i+1:]...)
			break
		}
	}
	return nil
}

type fakePublisher struct {
	mu         sync.Mutex
	published  []outbox.Event
	publishErr error
	done       chan error
	closed     bool
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{done: make(chan error)}
}

func (p *fakePublisher) Publish(_ context.Context, event outbox.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, event)
	return p.publishErr
}

func (p *fakePublisher) Done() <-chan error { return p.done }
func (p *fakePublisher) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func TestDispatchBatchPublishesThenMarks(t *testing.T) {
	event := validEvent(t)
	store := &fakeStore{events: []outbox.Event{event}}
	publisher := newFakePublisher()
	d := testDispatcher(store, nil)

	published, err := d.DispatchBatch(context.Background(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || len(publisher.published) != 1 || publisher.published[0].ID != event.ID {
		t.Fatalf("published count=%d events=%#v", published, publisher.published)
	}
	if len(store.marked) != 1 || store.marked[0] != event.ID {
		t.Fatalf("marked=%v", store.marked)
	}
}

func TestDispatchBatchPublishFailureLeavesEventUnpublished(t *testing.T) {
	event := validEvent(t)
	store := &fakeStore{events: []outbox.Event{event}}
	publisher := newFakePublisher()
	publisher.publishErr = cloudrabbit.ErrNack
	d := testDispatcher(store, nil)

	published, err := d.DispatchBatch(context.Background(), publisher)
	if published != 0 || err == nil {
		t.Fatalf("published=%d err=%v", published, err)
	}
	if len(store.marked) != 0 {
		t.Fatalf("marked=%v, want none", store.marked)
	}
}

func TestRunKeepsRetryingMalformedEventUntilShutdown(t *testing.T) {
	event := validEvent(t)
	event.Payload = json.RawMessage(`{"job_id":"wrong"}`)
	store := &fakeStore{events: []outbox.Event{event}}
	publisher := newFakePublisher()
	d := New(store, func(context.Context) (Publisher, error) {
		return publisher, nil
	}, time.Millisecond, time.Millisecond, 2*time.Millisecond, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		calls := store.listCalls
		store.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher stopped retrying malformed event")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(publisher.published) != 0 || len(store.marked) != 0 {
		t.Fatalf("published=%d marked=%d", len(publisher.published), len(store.marked))
	}
}

func TestDispatchBatchSkipsMalformedEventAndPublishesLaterEvent(t *testing.T) {
	event := validEvent(t)
	event.Payload = json.RawMessage(`{"job_id":"different-job"}`)
	valid := validEvent(t)
	valid.ID = "33333333-3333-3333-3333-333333333333"
	store := &fakeStore{events: []outbox.Event{event, valid}}
	publisher := newFakePublisher()
	d := testDispatcher(store, nil)

	published, err := d.DispatchBatch(context.Background(), publisher)
	if err != nil || published != 1 {
		t.Fatalf("published=%d err=%v", published, err)
	}
	if len(publisher.published) != 1 || publisher.published[0].ID != valid.ID {
		t.Fatalf("published=%#v", publisher.published)
	}
	if len(store.marked) != 1 || store.marked[0] != valid.ID || len(store.events) != 1 || store.events[0].ID != event.ID {
		t.Fatalf("marked=%v remaining=%#v", store.marked, store.events)
	}
}

func TestDispatchBatchAlreadyPublishedIsNotRepublished(t *testing.T) {
	store := &fakeStore{}
	publisher := newFakePublisher()
	d := testDispatcher(store, nil)

	published, err := d.DispatchBatch(context.Background(), publisher)
	if err != nil || published != 0 || len(publisher.published) != 0 {
		t.Fatalf("published=%d err=%v events=%d", published, err, len(publisher.published))
	}
}

func TestRunReconnectsAndStopsGracefully(t *testing.T) {
	store := &fakeStore{}
	publisher := newFakePublisher()
	var mu sync.Mutex
	attempts := 0
	connect := func(context.Context) (Publisher, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return nil, errors.New("broker unavailable")
		}
		return publisher, nil
	}
	d := New(store, connect, 5*time.Millisecond, time.Millisecond, 4*time.Millisecond, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		connected := attempts >= 3
		mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher did not reconnect")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop after cancellation")
	}
}

func TestDatabaseFailuresIncreaseBackoffAcrossSuccessfulBrokerReconnects(t *testing.T) {
	store := &fakeStore{listErr: errors.New("database unavailable")}
	var mu sync.Mutex
	var connectedAt []time.Time
	fourthConnection := make(chan struct{})
	connect := func(context.Context) (Publisher, error) {
		mu.Lock()
		connectedAt = append(connectedAt, time.Now())
		attempt := len(connectedAt)
		mu.Unlock()
		if attempt == 4 {
			close(fourthConnection)
		}
		return newFakePublisher(), nil
	}
	d := New(store, connect, time.Millisecond, 20*time.Millisecond, 80*time.Millisecond, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	select {
	case <-fourthConnection:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not make four attempts")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connectedAt) != 4 {
		t.Fatalf("connections=%d, want 4", len(connectedAt))
	}
	firstGap := connectedAt[1].Sub(connectedAt[0])
	secondGap := connectedAt[2].Sub(connectedAt[1])
	thirdGap := connectedAt[3].Sub(connectedAt[2])
	if firstGap < 15*time.Millisecond || secondGap < 35*time.Millisecond || thirdGap < 70*time.Millisecond {
		t.Fatalf("reconnect gaps=%s/%s/%s, want increasing backoff near 20ms/40ms/80ms", firstGap, secondGap, thirdGap)
	}
}

func validEvent(t *testing.T) outbox.Event {
	t.Helper()
	payload, err := json.Marshal(outbox.JobReadyPayload{JobID: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.Event{
		ID:          "22222222-2222-2222-2222-222222222222",
		EventType:   outbox.EventTypeJobReady,
		AggregateID: "11111111-1111-1111-1111-111111111111",
		Payload:     payload,
		CreatedAt:   time.Now(),
	}
}

func testDispatcher(store EventStore, connect Connector) *Dispatcher {
	return New(store, connect, time.Millisecond, time.Millisecond, 2*time.Millisecond, discardLogger())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
