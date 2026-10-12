package observability

import (
	"encoding/json"
	"sync"
	"time"
)

// Event is a live request notification pushed to dashboard subscribers.
type Event struct {
	Type             string  `json:"type,omitempty"`
	TS               string  `json:"ts"`
	KeyPrefix        string  `json:"key_prefix"`
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	LatencyMS        int     `json:"latency_ms"`
	Status           int     `json:"status"`
	CostUSD          float64 `json:"cost_usd"`
}

// Bus fans request events out to any number of dashboard subscribers.
//
// Subscribers that cannot keep up lose the oldest event rather than stalling
// the request path: live logs are diagnostic, and a slow dashboard reader must
// never add latency to traffic.
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]chan []byte
	nextID int
	// buffer is how many events a slow subscriber may fall behind before it
	// starts dropping the oldest.
	buffer int
}

// NewBus creates an event bus with the given per-subscriber buffer depth.
func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 256
	}
	return &Bus{subs: map[int]chan []byte{}, buffer: buffer}
}

// Subscribe registers a listener and returns its channel plus an unsubscribe
// function.
func (b *Bus) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, b.buffer)

	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			// Closing under the lock means a concurrent Publish cannot send on
			// a closed channel.
			close(existing)
		}
		b.mu.Unlock()
	}
}

// Publish delivers an event to every subscriber, dropping the oldest event
// for any subscriber that has fallen behind.
func (b *Bus) Publish(event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- payload:
		default:
			// Buffer full: make room by discarding the oldest event, then retry
			// once. The newest data is the valuable one.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- payload:
			default:
			}
		}
	}
}

// Subscribers reports how many dashboards are currently connected.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// NewEvent builds an event with the current timestamp filled in.
func NewEvent(keyPrefix, model, provider string, promptTokens, completionTokens, totalTokens, latencyMS, status int, cost float64) Event {
	return Event{
		TS:               time.Now().UTC().Format("2006-01-02T15:04:05"),
		KeyPrefix:        keyPrefix,
		Model:            model,
		Provider:         provider,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		LatencyMS:        latencyMS,
		Status:           status,
		CostUSD:          cost,
	}
}
