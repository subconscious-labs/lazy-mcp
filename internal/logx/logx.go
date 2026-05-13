// Package logx provides a minimal structured JSONL logger to an io.Writer.
package logx

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Logger writes JSONL log records.
type Logger struct {
	w  io.Writer
	mu sync.Mutex
}

// New creates a Logger writing to w.
func New(w io.Writer) *Logger { return &Logger{w: w} }

func (l *Logger) log(level string, msg string, kv []any) {
	rec := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			rec[k] = kv[i+1]
		}
	}
	b, _ := json.Marshal(rec)
	l.mu.Lock()
	l.w.Write(append(b, '\n')) //nolint:errcheck
	l.mu.Unlock()
}

func (l *Logger) Info(msg string, kv ...any)  { l.log("INFO", msg, kv) }
func (l *Logger) Warn(msg string, kv ...any)  { l.log("WARN", msg, kv) }
func (l *Logger) Error(msg string, kv ...any) { l.log("ERROR", msg, kv) }
func (l *Logger) Debug(msg string, kv ...any) { l.log("DEBUG", msg, kv) }
