package logx

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_WritesJSONL(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Info("hello", "k", "v")

	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, line)
	}
	if rec["level"] != "INFO" || rec["msg"] != "hello" || rec["k"] != "v" {
		t.Fatalf("unexpected record: %v", rec)
	}
	if _, ok := rec["ts"]; !ok {
		t.Fatalf("missing ts")
	}
}

func TestNewWithFile_NilSinkBehavesLikeNew(t *testing.T) {
	var buf bytes.Buffer
	l, closer := NewWithFile(&buf, nil)
	if closer != nil {
		t.Fatalf("nil sink should yield nil closer")
	}
	l.Info("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("stderr did not receive the log")
	}
}

func TestNewWithFile_WritesBoth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	closer := &fileSink{f}
	defer closer.Close()

	var buf bytes.Buffer
	l, c := NewWithFile(&buf, closer)
	if c == nil {
		t.Fatal("expected non-nil closer")
	}

	l.Info("dual", "n", 42)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "dual") {
		t.Fatalf("stderr missing line")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "dual") {
		t.Fatalf("file missing line: %q", data)
	}
}

// fileSink lets a plain *os.File satisfy the Sink interface in tests.
type fileSink struct{ *os.File }
