package meta

import (
	"strings"
	"testing"
	"time"
)

func TestPageStore_NextExpired(t *testing.T) {
	ps := NewPageStore(20 * time.Millisecond)
	content := strings.Repeat("x", 1000)
	_, cur, more := ps.Store(content, 100, "", "", "")
	if !more || cur == "" {
		t.Fatal("expected multi-page")
	}

	// Don't wait for the reaper: the entry should be rejected on read.
	time.Sleep(50 * time.Millisecond)

	_, _, _, _, _, ok := ps.Next(cur, "")
	if ok {
		t.Fatal("expired cursor should be rejected by Next, not only by the reaper")
	}
}
