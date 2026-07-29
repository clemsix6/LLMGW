package cliproxy

import (
	"context"
	"sync"
)

// usageCancellationContext delays cancellation visibility until its
// authenticated FIFO marker has been enqueued.
type usageCancellationContext struct {
	context.Context

	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (c *usageCancellationContext) Done() <-chan struct{} {
	return c.done
}

func (c *usageCancellationContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *usageCancellationContext) cancel(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
	c.mu.Unlock()
}

// withUsageCancellationMarker returns a context whose Done closes only after
// the cancel marker is synchronously appended to the SDK usage FIFO.
func withUsageCancellationMarker(
	parent context.Context,
	bridge *UsageBridge,
	requestID string,
) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	wrapped := &usageCancellationContext{
		Context: parent,
		done:    make(chan struct{}),
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-parent.Done():
			bridge.publishCancel(requestID)
			wrapped.cancel(parent.Err())
		case <-stop:
		}
	}()
	var once sync.Once
	stopWatching := func() {
		once.Do(func() {
			// When cancellation is already observable on the parent, do not race
			// it with the normal completion branch.
			if parent.Err() == nil {
				close(stop)
			}
			<-finished
		})
	}
	return wrapped, stopWatching
}
