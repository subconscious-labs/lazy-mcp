package meta

import (
	"strings"
	"testing"
	"time"
)

func BenchmarkPageStore_Small(b *testing.B) {
	ps := NewPageStore(time.Minute)
	content := strings.Repeat("hello world ", 100) // ~1.2 KiB
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = ps.Store(content, 256)
	}
}

func BenchmarkPageStore_Large(b *testing.B) {
	ps := NewPageStore(time.Minute)
	content := strings.Repeat("hello world ", 10000) // ~120 KiB
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = ps.Store(content, 4096)
	}
}

func BenchmarkTruncateBytes_ASCII(b *testing.B) {
	s := strings.Repeat("x", 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = truncateBytes(s, 4096)
	}
}

func BenchmarkTruncateBytes_UTF8(b *testing.B) {
	s := strings.Repeat("привет мир ", 10000) // ~200 KiB
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = truncateBytes(s, 4096)
	}
}
