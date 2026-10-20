package config

import (
	"sync"
	"testing"
)

func TestHolder_GetReturnsInitial(t *testing.T) {
	cfg := &Config{Version: 1}
	h := NewHolder(cfg)
	if got := h.Get(); got != cfg {
		t.Fatalf("Get: got %p want %p", got, cfg)
	}
}

func TestHolder_SwapReturnsPrevious(t *testing.T) {
	old := &Config{Version: 1}
	newCfg := &Config{Version: 2}
	h := NewHolder(old)

	prev := h.Swap(newCfg)
	if prev != old {
		t.Fatalf("Swap should return previous config")
	}
	if got := h.Get(); got != newCfg {
		t.Fatalf("Get after Swap: got %p want %p", got, newCfg)
	}
}

func TestHolder_ConcurrentGetSwap(t *testing.T) {
	h := NewHolder(&Config{Version: 0})

	const readers = 8
	const iterations = 100

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: Get in a tight loop, must never see nil.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if h.Get() == nil {
						t.Error("Get returned nil")
						return
					}
				}
			}
		}()
	}

	// Swapper: replace the config many times.
	for i := 1; i <= iterations; i++ {
		h.Swap(&Config{Version: i})
	}

	close(stop)
	wg.Wait()

	if h.Get().Version != iterations {
		t.Fatalf("final version: got %d want %d", h.Get().Version, iterations)
	}
}
