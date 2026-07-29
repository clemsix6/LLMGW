package integration

import (
	"errors"
	"fmt"
	"time"
)

// stopService cancels and waits for the single embedded SDK lifecycle.
func (h *Harness) stopService() error {
	h.stopOnce.Do(func() {
		h.stopErr = h.stopServiceOnce()
	})
	return h.stopErr
}

// stopServiceOnce performs the single embedded SDK cancellation and wait.
func (h *Harness) stopServiceOnce() error {
	if h.cancel != nil {
		h.cancel()
	}
	if h.done == nil {
		return nil
	}
	select {
	case err := <-h.done:
		if err != nil {
			return fmt.Errorf("run embedded proxy:\n%w", err)
		}
		return nil
	case <-time.After(harnessShutdownTimeout):
		return errors.New("embedded proxy shutdown timed out")
	}
}
