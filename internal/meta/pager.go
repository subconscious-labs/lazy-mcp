package meta

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// pagedResult holds all pages of a single call result.
type pagedResult struct {
	pages     []string
	createdAt time.Time
}

// PageStore stores cursor → pagedResult with TTL-based expiry.
// All methods are safe for concurrent use.
//
// A cursor has the form "<session-id>.<page-index>" where page-index is
// 1-based. The first page is returned directly by Store; subsequent pages
// are addressed by cursor and are idempotent — repeating a request with
// the same cursor yields the same page.
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
	if ttl > 0 {
		go ps.reaper()
	}
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
		cut := pageSize
		for i := pageSize - 1; i > pageSize/2; i-- {
			if content[i] == '\n' {
				cut = i + 1
				break
			}
		}
		// Back off to a rune boundary if no newline was found nearby.
		// Stop at 1: cutting mid-rune is better than spinning forever.
		if cut > 0 && content[cut-1] != '\n' {
			for cut > 1 && !utf8.RuneStart(content[cut]) {
				cut--
			}
		}
		pages = append(pages, content[:cut])
		content = content[cut:]
	}

	if len(pages) == 1 {
		return pages[0], "", false
	}

	id := newSessionID()
	ps.mu.Lock()
	ps.entries[id] = &pagedResult{
		pages:     pages,
		createdAt: time.Now(),
	}
	ps.mu.Unlock()

	return pages[0], cursorFor(id, 2), true
}

// Next returns the page indicated by cursor.
func (ps *PageStore) Next(cursor string) (page, nextCursor string, hasMore bool, ok bool) {
	id, idx, err := parseCursor(cursor)
	if err != nil {
		return "", "", false, false
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	pr, exists := ps.entries[id]
	if !exists {
		return "", "", false, false
	}
	if idx < 1 || idx > len(pr.pages) {
		return "", "", false, false
	}

	pr.createdAt = time.Now() // refresh TTL on access
	page = pr.pages[idx-1]
	if idx < len(pr.pages) {
		return page, cursorFor(id, idx+1), true, true
	}
	return page, "", false, true
}

func (ps *PageStore) reaper() {
	if ps.ttl <= 0 {
		return
	}
	interval := ps.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
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

func newSessionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cursorFor(id string, idx int) string {
	return id + "." + strconv.Itoa(idx)
}

func parseCursor(c string) (id string, idx int, err error) {
	i := strings.LastIndexByte(c, '.')
	if i < 0 {
		return "", 0, fmt.Errorf("malformed cursor")
	}
	idx, err = strconv.Atoi(c[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("malformed cursor index: %w", err)
	}
	return c[:i], idx, nil
}
