package event

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifier_Subscribe_And_Notify(t *testing.T) {
	n := NewNotifier()
	var called atomic.Int32

	n.Subscribe("test", func(ctx context.Context, e Event) error {
		called.Add(1)
		return nil
	})

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "run001",
		FromState: "brainstorm",
		ToState:   "strategized",
		Timestamp: time.Now(),
	}

	err := n.Notify(context.Background(), e)
	if err != nil {
		t.Errorf("Notify returned error: %v", err)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", called.Load())
	}
}

func TestNotifier_MultipleHandlers(t *testing.T) {
	n := NewNotifier()
	var order []string

	n.Subscribe("first", func(ctx context.Context, e Event) error {
		order = append(order, "first")
		return nil
	})
	n.Subscribe("second", func(ctx context.Context, e Event) error {
		order = append(order, "second")
		return nil
	})

	n.Notify(context.Background(), Event{Source: SourcePhase, Type: "advance"})

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("handler order = %v, want [first, second]", order)
	}
}

func TestNotifier_HandlerError_DoesNotBlockOthers(t *testing.T) {
	n := NewNotifier()
	var secondCalled bool

	n.Subscribe("failing", func(ctx context.Context, e Event) error {
		return errors.New("boom")
	})
	n.Subscribe("succeeding", func(ctx context.Context, e Event) error {
		secondCalled = true
		return nil
	})

	err := n.Notify(context.Background(), Event{Source: SourcePhase, Type: "advance"})

	if err == nil {
		t.Error("expected error from failing handler")
	}
	if !secondCalled {
		t.Error("second handler was not called after first handler failed")
	}
}

func TestNotifier_NoHandlers(t *testing.T) {
	n := NewNotifier()

	err := n.Notify(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Errorf("Notify with no handlers returned error: %v", err)
	}
}

func TestNotifier_HandlerCount(t *testing.T) {
	n := NewNotifier()
	if n.HandlerCount() != 0 {
		t.Errorf("HandlerCount = %d, want 0", n.HandlerCount())
	}

	n.Subscribe("a", func(ctx context.Context, e Event) error { return nil })
	n.Subscribe("b", func(ctx context.Context, e Event) error { return nil })

	if n.HandlerCount() != 2 {
		t.Errorf("HandlerCount = %d, want 2", n.HandlerCount())
	}
}
