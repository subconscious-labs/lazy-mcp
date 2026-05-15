package meta

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// pagedResult holds pre-split pages for a single call result.
type pagedResult struct {
	pages     []string
	createdAt time.Time
}

// PageStore stores cursor → pagedResult with TTL-based expiry.
// All methods are safe for concurrent use.
type PageStore struct {
	mu      sync.Mutex
	entries map[string]*pagedResult
	ttl     time.Duration
}

// NewPageStore creates a PageStore with the given TTL.
// A background goroutine cleans up expired cursors every ttl/2.
func NewPageStore(ttl time.Duration) *PageStore {
	ps := &PageStore{
		entries: make(map[string]*pagedResult),
		ttl:     ttl,
	}
	go ps.reaper()
	return ps
}

// Store splits content into pages of pageSize bytes and returns the first
// page plus a cursor for subsequent pages (empty cursor if only one page).
func (ps *PageStore) Store(content string, pageSize int) (firstPage, cursor string, hasMore bool) {
	if pageSize <= 0 || len(content) <= pageSize {
		return content, "", false
	}

	var pages []string
	for len(content) > 0 {
		if len(content) <= pageSize {
			pages = append(pages, content)
			break
		}
		// Split on newline boundary near pageSize to avoid cutting JSON mid-token.
		cut := pageSize
		for i := pageSize - 1; i > pageSize/2; i-- {
			if content[i] == '\n' {
				cut = i + 1
				break
			}
		}
		pages = append(pages, content[:cut])
		content = content[cut:]
	}

	if len(pages) == 1 {
		return pages[0], "", false
	}

	cursor = newCursor()
	ps.mu.Lock()
	ps.entries[cursor] = &pagedResult{
		pages:     pages[1:], // first page returned directly
		createdAt: time.Now(),
	}
	ps.mu.Unlock()

	return pages[0], cursor, true
}

// Next returns the next page for cursor. Returns "", false if cursor unknown or expired.
func (ps *PageStore) Next(cursor string) (page, nextCursor string, hasMore bool, ok bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	pr, exists := ps.entries[cursor]
	if !exists {
		return "", "", false, false
	}

	if len(pr.pages) == 0 {
		delete(ps.entries, cursor)
		return "", "", false, true
	}

	page = pr.pages[0]
	pr.pages = pr.pages[1:]

	if len(pr.pages) == 0 {
		delete(ps.entries, cursor)
		return page, "", false, true
	}

	// Reuse the same cursor for remaining pages.
	pr.createdAt = time.Now() // refresh TTL on access
	return page, cursor, true, true
}

func (ps *PageStore) reaper() {
	ticker := time.NewTicker(ps.ttl / 2)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		ps.mu.Lock()
		for k, v := range ps.entries {
			if now.Sub(v.createdAt) > ps.ttl {
				delete(ps.entries, k)
			}
		}
		ps.mu.Unlock()
	}
}

func newCursor() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
