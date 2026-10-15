package meta

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTruncateBytes_ShortStringUntouched(t *testing.T) {
	s := "hello"
	if got := truncateBytes(s, 100); got != s {
		t.Fatalf("want %q, got %q", s, got)
	}
}

func TestTruncateBytes_ZeroLimitNoLimit(t *testing.T) {
	s := strings.Repeat("x", 1000)
	if got := truncateBytes(s, 0); got != s {
		t.Fatalf("limit=0 should be no-op")
	}
}

func TestTruncateBytes_CutsAtRuneBoundary(t *testing.T) {
	// "Привет" - 12 bytes, 6 runes of 2 bytes each.
	s := "Привет"
	// Limit = 5 bytes: cuts inside the 3rd rune. Should back off to 4 bytes.
	got := truncateBytes(s, 5)
	body := strings.TrimSuffix(got, "\n… [truncated]")
	if !utf8.ValidString(body) {
		t.Fatalf("output is not valid UTF-8: %q", got)
	}
	if len(body) > 5 {
		t.Fatalf("body exceeds limit: %d bytes", len(body))
	}
}

func TestSplitRef(t *testing.T) {
	tests := []struct {
		in           string
		server, tool string
		wantErr      bool
	}{
		{"ssh/ssh_exec", "ssh", "ssh_exec", false},
		{"a/b/c", "a", "b/c", false},
		{"no-slash", "", "", true},
		{"", "", "", true},
	}
	for _, tt := range tests {
		s, tool, err := splitRef(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("splitRef(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (s != tt.server || tool != tt.tool) {
			t.Errorf("splitRef(%q) = %q, %q; want %q, %q", tt.in, s, tool, tt.server, tt.tool)
		}
	}
}

func TestPageStore_SinglePageNoCursor(t *testing.T) {
	ps := NewPageStore(time.Minute)
	first, cur, more := ps.Store("short", 100)
	if first != "short" || cur != "" || more {
		t.Fatalf("got %q, %q, %v", first, cur, more)
	}
}

func TestPageStore_MultiPage(t *testing.T) {
	ps := NewPageStore(time.Minute)
	// Force splits on newline boundaries so we don't rely on mid-string cuts.
	content := strings.Repeat("line\n", 100) // 500 bytes

	first, cur, more := ps.Store(content, 120)
	if !more || cur == "" {
		t.Fatalf("expected multi-page, got more=%v cur=%q", more, cur)
	}
	if len(first) == 0 {
		t.Fatal("first page empty")
	}

	// Walk through remaining pages.
	total := len(first)
	for cur != "" {
		page, next, more, ok := ps.Next(cur)
		if !ok {
			t.Fatal("cursor rejected")
		}
		total += len(page)
		if !more {
			cur = ""
		} else {
			cur = next
		}
	}
	if total != len(content) {
		t.Fatalf("reassembled length %d, want %d", total, len(content))
	}
}

func TestPageStore_UnknownCursor(t *testing.T) {
	ps := NewPageStore(time.Minute)
	_, _, _, ok := ps.Next("nope")
	if ok {
		t.Fatal("unknown cursor should be rejected")
	}
}
