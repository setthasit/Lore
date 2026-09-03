package services

import (
	"sync"
	"time"

	"github.com/setthasit/Lore/internal/entities"
)

const syncEventBuffer = 64

type syncEventBus struct {
	mu          sync.Mutex
	subscribers map[chan entities.SyncEvent]struct{}
}

func (b *syncEventBus) subscribe() (<-chan entities.SyncEvent, func()) {
	events := make(chan entities.SyncEvent, syncEventBuffer)

	b.mu.Lock()
	if b.subscribers == nil {
		b.subscribers = make(map[chan entities.SyncEvent]struct{})
	}
	b.subscribers[events] = struct{}{}
	b.mu.Unlock()

	return events, func() { b.unsubscribe(events) }
}

func (b *syncEventBus) unsubscribe(events chan entities.SyncEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, subscribed := b.subscribers[events]; !subscribed {
		return
	}

	delete(b.subscribers, events)
	close(events)
}

func (b *syncEventBus) publish(event entities.SyncEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for events := range b.subscribers {
		sendDroppingOldest(events, event)
	}
}

func sendDroppingOldest(events chan entities.SyncEvent, event entities.SyncEvent) {
	select {
	case events <- event:
		return
	default:
	}

	select {
	case <-events:
	default:
	}

	select {
	case events <- event:
	default:
	}
}

type syncProgress struct {
	bus       *syncEventBus
	now       func() time.Time
	documents int64
	chunks    int64
}

func (p *syncProgress) emit(source string, phase entities.SyncPhase, err error) {
	p.bus.publish(entities.SyncEvent{
		Source:    source,
		Phase:     phase,
		Documents: p.documents,
		Chunks:    p.chunks,
		Err:       err,
		At:        p.now(),
	})
}
