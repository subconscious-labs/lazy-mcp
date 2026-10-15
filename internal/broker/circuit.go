package broker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrCircuitOpen is returned when a backend's circuit breaker is open and
// the cooldown has not elapsed yet.
var ErrCircuitOpen = errors.New("backend circuit open")

type circuitState int

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

func (s circuitState) String() string {
	switch s {
	case circuitClosed:
		return "closed"
	case circuitOpen:
		return "open"
	case circuitHalfOpen:
		return "half-open"
	}
	return "unknown"
}

// circuit is a single-backend circuit breaker.
type circuit struct {
	mu sync.Mutex

	state            circuitState
	consecutiveFails int
	openedAt         time.Time

	threshold int
	cooldown  time.Duration
}

func newCircuit(threshold int, cooldown time.Duration) *circuit {
	return &circuit{threshold: threshold, cooldown: cooldown}
}

// allow returns nil if a call may proceed. In open state it transitions to
// half-open once cooldown has elapsed and lets exactly one probe through;
// concurrent probes are rejected.
func (c *circuit) allow() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case circuitClosed:
		return nil

	case circuitOpen:
		remaining := c.cooldown - time.Since(c.openedAt)
		if remaining > 0 {
			return fmt.Errorf("%w (retry in %s)", ErrCircuitOpen, remaining.Round(time.Second))
		}
		c.state = circuitHalfOpen
		return nil // this caller is the probe

	case circuitHalfOpen:
		return fmt.Errorf("%w (probe in flight)", ErrCircuitOpen)
	}
	return nil
}

func (c *circuit) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = circuitClosed
	c.consecutiveFails = 0
	c.openedAt = time.Time{}
}

func (c *circuit) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveFails++
	// In half-open, any failure re-opens immediately.
	if c.state == circuitHalfOpen || c.consecutiveFails >= c.threshold {
		c.state = circuitOpen
		c.openedAt = time.Now()
	}
}

func (c *circuit) snapshot() (circuitState, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.consecutiveFails
}
