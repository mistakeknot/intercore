package event

import (
	"context"
	"fmt"
	"sync"
)

// Handler processes an event. Errors are logged but don't fail the parent operation.
type Handler func(ctx context.Context, e Event) error

// Notifier dispatches events to registered handlers.
type Notifier struct {
	mu       sync.RWMutex
	handlers []namedHandler
}

type namedHandler struct {
	name    string
	handler Handler
}

// NewNotifier creates a new event notifier.
func NewNotifier() *Notifier {
	return &Notifier{}
}

// Subscribe registers a named handler. Name is used for error logging.
func (n *Notifier) Subscribe(name string, h Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers = append(n.handlers, namedHandler{name: name, handler: h})
}

// Notify dispatches an event to all handlers synchronously.
// Handler errors are collected but do not stop dispatch to remaining handlers.
// Returns the first error encountered (if any) for logging purposes only.
func (n *Notifier) Notify(ctx context.Context, e Event) error {
	n.mu.RLock()
	handlers := make([]namedHandler, len(n.handlers))
	copy(handlers, n.handlers)
	n.mu.RUnlock()

	var firstErr error
	for _, nh := range handlers {
		if err := nh.handler(ctx, e); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("handler %s: %w", nh.name, err)
			}
		}
	}
	return firstErr
}

// HandlerCount returns the number of registered handlers.
func (n *Notifier) HandlerCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.handlers)
}
